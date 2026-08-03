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
}

type Poller struct {
	Store  *store.Store
	Client *Client
	Now    func() time.Time
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
	if cursor, err := p.Store.GitHubPollCursor(ctx, repository); err == nil && cursor.NextAttemptAt.After(now) {
		return PollResult{}, store.ErrNotReady
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return PollResult{}, err
	}
	result, err := p.poll(ctx, repository, now)
	if err != nil {
		if recordErr := p.Store.RecordGitHubPollFailure(ctx, repository, now); recordErr != nil {
			return PollResult{}, errors.Join(err, recordErr)
		}
		return PollResult{}, err
	}
	if err := p.Store.RecordGitHubPollSuccess(ctx, repository, now); err != nil {
		return PollResult{}, err
	}
	return result, nil
}

func (p Poller) poll(ctx context.Context, repository string, now time.Time) (PollResult, error) {
	deliveries, err := p.Store.PendingTicketDeliveries(ctx, repository)
	if err != nil {
		return PollResult{}, err
	}
	result := PollResult{Deliveries: len(deliveries)}
	reconciler := DeliveredReconciler{Store: p.Store, Client: p.Client}
	for _, delivery := range deliveries {
		terminal, err := reconciler.ReconcileTicket(ctx, delivery)
		if err != nil {
			return PollResult{}, err
		}
		if terminal {
			continue
		}
		events, err := p.Client.ActionablePullRequestFeedback(ctx, repository, delivery.PullRequestNumber)
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
	}
	return result, nil
}
