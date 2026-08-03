package github

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/skyhuang233/workflow/internal/store"
)

type PollResult struct {
	Deliveries int
	Feedback   int
	Checks     int
}

type ReviewLauncher func(context.Context, store.TicketClaim, string) error

type Poller struct {
	Store                 *store.Store
	Client                *Client
	Now                   func() time.Time
	LaunchReview          ReviewLauncher
	MaxFailures           int
	MaxWorkerAttempts     int
	FullReconcileInterval time.Duration
}

func (p Poller) now() time.Time {
	if p.Now != nil {
		return p.Now().UTC()
	}
	return time.Now().UTC()
}

func (p Poller) Poll(ctx context.Context, repository string) (PollResult, error) {
	if p.Store == nil || p.Client == nil {
		return PollResult{}, fmt.Errorf("GitHub poller dependencies are incomplete")
	}
	if err := ValidateRepository(repository); err != nil {
		return PollResult{}, err
	}
	now := p.now()
	cursor, err := p.Store.GitHubPollCursor(ctx, repository)
	if err == nil {
		if cursor.ConsecutiveFailures >= p.maxFailures() {
			return PollResult{}, store.ErrNeedsAttention
		}
		if cursor.NextAttemptAt.After(now) {
			return PollResult{}, store.ErrNotReady
		}
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return PollResult{}, err
	}
	full := err != nil || cursor.LastFullReconcileAt.IsZero() || now.Sub(cursor.LastFullReconcileAt) >= p.fullReconcileInterval()
	result, err := p.poll(ctx, repository, now, cursor.LastSuccessAt, full)
	if err != nil {
		if recordErr := p.Store.RecordGitHubPollFailure(ctx, repository, now); recordErr != nil {
			return PollResult{}, errors.Join(err, recordErr)
		}
		updated, cursorErr := p.Store.GitHubPollCursor(ctx, repository)
		if cursorErr == nil && updated.ConsecutiveFailures >= p.maxFailures() {
			if attentionErr := p.Store.MarkRepositoryNeedsAttention(ctx, repository, now); attentionErr != nil {
				return PollResult{}, errors.Join(err, attentionErr)
			}
			return PollResult{}, store.ErrNeedsAttention
		}
		return PollResult{}, err
	}
	if err := p.Store.RecordGitHubPollSuccess(ctx, repository, now, full); err != nil {
		return PollResult{}, err
	}
	return result, nil
}

func (p Poller) maxFailures() int {
	if p.MaxFailures > 0 {
		return p.MaxFailures
	}
	return store.DefaultMaxWorkerAttempts
}

func (p Poller) fullReconcileInterval() time.Duration {
	if p.FullReconcileInterval > 0 {
		return p.FullReconcileInterval
	}
	return 15 * time.Minute
}

func (p Poller) poll(ctx context.Context, repository string, now, since time.Time, full bool) (PollResult, error) {
	deliveries, err := p.Store.PendingTicketDeliveries(ctx, repository)
	if err != nil {
		return PollResult{}, err
	}
	result := PollResult{Deliveries: len(deliveries)}
	updatedPullRequests, err := p.Client.UpdatedPullRequestsSince(ctx, repository, since, full)
	if err != nil {
		return PollResult{}, err
	}
	reconciler := DeliveredReconciler{Store: p.Store, Client: p.Client}
	for _, delivery := range deliveries {
		if full || hasPullRequestUpdate(updatedPullRequests, delivery.PullRequestNumber) {
			terminal, err := reconciler.ReconcileTicket(ctx, delivery)
			if err != nil {
				return PollResult{}, err
			}
			if !terminal {
				events, err := p.Client.ActionablePullRequestFeedbackSince(ctx, repository, delivery.PullRequestNumber, since, full)
				if err != nil {
					return PollResult{}, err
				}
				feedback := make([]store.ReviewFeedback, 0, len(events))
				for _, event := range events {
					feedback = append(feedback, store.ReviewFeedback{Source: event.Source, EventID: event.EventID, Author: event.Author, Body: event.Body})
				}
				inserted, err := p.Store.RecordReviewFeedback(ctx, delivery.VersionID, delivery.IssueID, feedback, now)
				if err != nil {
					return PollResult{}, err
				}
				result.Feedback += inserted
				if p.LaunchReview != nil {
					claim, prompt, claimErr := p.Store.ClaimQueuedReviewRevision(ctx, delivery.VersionID, delivery.IssueID, 30*time.Minute, now, p.MaxWorkerAttempts)
					if claimErr == nil {
						if err := p.LaunchReview(ctx, claim, prompt); err != nil {
							return PollResult{}, err
						}
					} else if !errors.Is(claimErr, store.ErrNotReady) && !errors.Is(claimErr, store.ErrNotFound) {
						return PollResult{}, claimErr
					}
				}
			}
		}
		checks, etag, changed, err := p.Client.PullRequestChecksIfChanged(ctx, repository, delivery.CandidateCommit, delivery.ChecksETag, full)
		if err != nil {
			return PollResult{}, err
		}
		if changed {
			updated, err := p.Store.RecordPullRequestChecks(ctx, delivery.VersionID, delivery.IssueID, checks, now)
			if err != nil {
				return PollResult{}, err
			}
			if err := p.Store.RecordPullRequestChecksETag(ctx, delivery.VersionID, delivery.IssueID, etag); err != nil {
				return PollResult{}, err
			}
			result.Checks += updated
		}
	}
	return result, nil
}

func hasPullRequestUpdate(updated map[int64]struct{}, number int64) bool {
	_, ok := updated[number]
	return ok
}
