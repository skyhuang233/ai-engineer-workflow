package github

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/skyhuang233/workflow/internal/delivery"
	"github.com/skyhuang233/workflow/internal/plan"
	"github.com/skyhuang233/workflow/internal/store"
)

type PollResult struct {
	Deliveries int
	Feedback   int
	Checks     int
}

type ReviewLauncher func(context.Context, store.TicketClaim, string) error
type ControlPass func(context.Context) error
type BootstrapControlPass func(context.Context, bool) error

type WorkflowInboxProjector interface {
	ProjectWorkflowInbox(context.Context, string, []plan.WorkflowQuestion) error
}

const githubPollLeaseTTL = 5 * time.Minute

type githubPollLease struct {
	repository string
	token      string
}

type githubPollLeaseContextKey struct{}

type Poller struct {
	Store                 *store.Store
	Client                *Client
	Now                   func() time.Time
	LaunchReview          ReviewLauncher
	InboxProjector        WorkflowInboxProjector
	MaxFailures           int
	MaxWorkerAttempts     int
	MaxParallelRuns       int
	FullReconcileInterval time.Duration
}

func (p Poller) now() time.Time {
	if p.Now != nil {
		return p.Now().UTC()
	}
	return time.Now().UTC()
}

func (p Poller) Poll(ctx context.Context, repository string) (PollResult, error) {
	return p.PollWith(ctx, repository, nil)
}

func (p Poller) RecordFailure(ctx context.Context, repository string, cause error) (PollResult, error) {
	return p.recordFailure(ctx, repository, p.now(), cause)
}

func (p Poller) RecordTerminalFailure(ctx context.Context, repository string, cause error) (PollResult, error) {
	if p.Store == nil {
		return PollResult{}, errors.Join(cause, fmt.Errorf("GitHub poller store is unavailable"), store.ErrNeedsAttention)
	}
	if err := ValidateRepository(repository); err != nil {
		return PollResult{}, errors.Join(cause, err, store.ErrNeedsAttention)
	}
	return PollResult{}, p.terminalFailure(ctx, repository, p.now(), cause)
}

func (p Poller) Ready(ctx context.Context, repository string) error {
	if p.Store == nil {
		return fmt.Errorf("GitHub poller store is unavailable")
	}
	if err := ValidateRepository(repository); err != nil {
		return err
	}
	return p.readyAt(ctx, repository, p.now())
}

// AcquireLease admits one repository poll atomically with its NextAttemptAt
// gate. The returned context carries the fencing token through credential
// admission and PollWithBootstrap; callers must invoke release.
func (p Poller) AcquireLease(ctx context.Context, repository string) (context.Context, func() error, error) {
	if p.Store == nil {
		return ctx, nil, fmt.Errorf("GitHub poller store is unavailable")
	}
	if err := ValidateRepository(repository); err != nil {
		return ctx, nil, err
	}
	if lease, ok := ctx.Value(githubPollLeaseContextKey{}).(githubPollLease); ok && lease.repository == repository {
		if err := p.Store.RenewGitHubPollLease(ctx, repository, lease.token, p.now(), githubPollLeaseTTL); err != nil {
			return ctx, nil, err
		}
		return ctx, func() error { return nil }, nil
	}
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return ctx, nil, fmt.Errorf("create GitHub poll lease token: %w", err)
	}
	token := hex.EncodeToString(bytes)
	if err := p.Store.AcquireGitHubPollLease(ctx, repository, token, p.now(), githubPollLeaseTTL); err != nil {
		return ctx, nil, err
	}
	boundedCtx, cancelLease := context.WithTimeout(ctx, githubPollLeaseTTL-30*time.Second)
	leaseCtx := context.WithValue(boundedCtx, githubPollLeaseContextKey{}, githubPollLease{repository: repository, token: token})
	release := func() error {
		cancelLease()
		persistenceCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		return p.Store.ReleaseGitHubPollLease(persistenceCtx, repository, token, p.now())
	}
	return leaseCtx, release, nil
}

func (p Poller) ConsumeBootstrapEligibility(ctx context.Context, repository string) error {
	if p.Store == nil {
		return fmt.Errorf("GitHub poller store is unavailable")
	}
	if err := ValidateRepository(repository); err != nil {
		return err
	}
	persistenceCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	return p.Store.ConsumeGitHubPollBootstrapEligibility(persistenceCtx, repository, p.now())
}

func (p Poller) PollWith(ctx context.Context, repository string, before ControlPass) (PollResult, error) {
	var control BootstrapControlPass
	if before != nil {
		control = func(ctx context.Context, _ bool) error {
			return before(ctx)
		}
	}
	return p.PollWithBootstrap(ctx, repository, nil, control)
}

func (p Poller) PollWithBootstrap(ctx context.Context, repository string, bootstrap ControlPass, before BootstrapControlPass) (PollResult, error) {
	if p.Store == nil || p.Client == nil {
		return PollResult{}, fmt.Errorf("GitHub poller dependencies are incomplete")
	}
	if err := ValidateRepository(repository); err != nil {
		return PollResult{}, err
	}
	leaseCtx, release, err := p.AcquireLease(ctx, repository)
	if err != nil {
		return PollResult{}, err
	}
	result, pollErr := p.pollWithBootstrapLeased(leaseCtx, repository, bootstrap, before)
	if releaseErr := release(); releaseErr != nil {
		pollErr = errors.Join(pollErr, releaseErr)
	}
	return result, pollErr
}

func (p Poller) pollWithBootstrapLeased(ctx context.Context, repository string, bootstrap ControlPass, before BootstrapControlPass) (PollResult, error) {
	now := p.now()
	if err := p.readyAt(ctx, repository, now); err != nil {
		if errors.Is(err, store.ErrNotReady) {
			return PollResult{}, err
		}
		return PollResult{}, err
	}
	cursor, err := p.Store.GitHubPollCursor(ctx, repository)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return PollResult{}, err
	}
	if err := p.routeInboxAnswers(ctx, repository); err != nil {
		return p.recordFailure(ctx, repository, now, err)
	}
	bootstrapped := false
	cursor, err = p.Store.GitHubPollCursor(ctx, repository)
	if err == nil {
		if cursor.NeedsAttention() {
			return PollResult{}, p.terminalFailure(ctx, repository, now, fmt.Errorf("GitHub polling is paused pending human recovery"))
		}
		if cursor.ConsecutiveFailures >= p.maxFailures() {
			active, activeErr := p.Store.HasActiveDeliveryPlan(ctx, repository)
			if activeErr != nil {
				return PollResult{}, p.terminalFailure(ctx, repository, now, activeErr)
			}
			if active || bootstrap == nil || cursor.FailureKind != store.GitHubPollFailurePreActivationInboxConflict || cursor.RecoveryState != store.GitHubPollRecoveryAvailable {
				return PollResult{}, p.finishExhaustedFailure(ctx, repository, now, fmt.Errorf("GitHub poll bootstrap recovery is unavailable"))
			}
			claimed, claimErr := p.Store.ClaimGitHubPollBootstrapRecovery(ctx, repository, p.maxFailures(), now)
			if claimErr != nil {
				return PollResult{}, p.terminalFailure(ctx, repository, now, claimErr)
			}
			if !claimed {
				return PollResult{}, store.ErrNeedsAttention
			}
			if bootstrapErr := bootstrap(ctx); bootstrapErr != nil {
				return p.recordFailure(ctx, repository, now, bootstrapErr)
			}
			bootstrapped = true
			active, activeErr = p.Store.HasActiveDeliveryPlan(ctx, repository)
			if activeErr != nil {
				return PollResult{}, p.terminalFailure(ctx, repository, now, activeErr)
			}
			if !active {
				return p.recordFailure(ctx, repository, now, fmt.Errorf("plan bootstrap did not activate a delivery plan"))
			}
			recovered, err := p.Store.RecoverGitHubPollAfterBootstrap(ctx, repository, now)
			if err != nil {
				return PollResult{}, p.terminalFailure(ctx, repository, now, err)
			}
			if !recovered {
				return PollResult{}, p.terminalFailure(ctx, repository, now, fmt.Errorf("GitHub poll bootstrap recovery claim was lost"))
			}
			cursor.ConsecutiveFailures = 0
			cursor.FailureKind = ""
			cursor.RecoveryState = store.GitHubPollRecoveryCompleted
			cursor.NextAttemptAt = now
		}
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return PollResult{}, err
	}
	full := err != nil || cursor.LastFullReconcileAt.IsZero() || now.Sub(cursor.LastFullReconcileAt) >= p.fullReconcileInterval()
	if before != nil {
		if err := before(ctx, bootstrapped); err != nil {
			return p.recordFailure(ctx, repository, now, err)
		}
	}
	if err := p.projectWorkflowInbox(ctx, repository); err != nil {
		failureKind, classificationErr := p.inboxProjectionFailureKind(ctx, repository, err)
		if classificationErr != nil {
			return p.recordFailure(ctx, repository, now, errors.Join(err, classificationErr))
		}
		return p.recordFailureWithKind(ctx, repository, now, failureKind, err)
	}
	result, err := p.poll(ctx, repository, now, cursor.LastSuccessAt, full)
	if err != nil {
		return p.recordFailure(ctx, repository, now, err)
	}
	if err := p.Store.RecordGitHubPollSuccess(ctx, repository, now, full); err != nil {
		return p.recordFailure(ctx, repository, now, err)
	}
	return result, nil
}

func (p Poller) readyAt(ctx context.Context, repository string, now time.Time) error {
	cursor, err := p.Store.GitHubPollCursor(ctx, repository)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if cursor.NextAttemptAt.After(now) {
		return store.ErrNotReady
	}
	return nil
}

func (p Poller) recordFailure(ctx context.Context, repository string, now time.Time, cause error) (PollResult, error) {
	return p.recordFailureWithKind(ctx, repository, now, "", cause)
}

func (p Poller) recordFailureWithKind(ctx context.Context, repository string, now time.Time, failureKind store.GitHubPollFailureKind, cause error) (PollResult, error) {
	persistenceCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if isPollCredentialFailure(cause) {
		consumeErr := p.Store.ConsumeGitHubPollBootstrapEligibility(persistenceCtx, repository, now)
		pauseErr := p.Store.PauseGatewayWrites(persistenceCtx, "Gateway Credential is unavailable; replace and verify it to resume writes", now)
		return PollResult{}, errors.Join(cause, consumeErr, pauseErr)
	}
	var githubError *apiError
	if errors.As(cause, &githubError) && githubError.RetryAt.After(now) {
		updated, err := p.Store.DeferGitHubPollWithCursor(persistenceCtx, repository, githubError.RetryAt, now)
		if err != nil {
			return PollResult{}, p.terminalFailure(persistenceCtx, repository, now, cause, err)
		}
		if updated.ConsecutiveFailures >= p.maxFailures() {
			return PollResult{}, p.finishExhaustedFailure(persistenceCtx, repository, now, cause)
		}
		return PollResult{}, cause
	}
	updated, recordErr := p.Store.AdvanceGitHubPollFailure(persistenceCtx, repository, now, failureKind)
	if recordErr != nil {
		return PollResult{}, p.terminalFailure(persistenceCtx, repository, now, cause, recordErr)
	}
	if updated.ConsecutiveFailures >= p.maxFailures() {
		if updated.FailureKind == store.GitHubPollFailurePreActivationInboxConflict && updated.RecoveryState == store.GitHubPollRecoveryAvailable {
			active, activeErr := p.Store.HasActiveDeliveryPlan(persistenceCtx, repository)
			if activeErr != nil {
				return PollResult{}, p.terminalFailure(persistenceCtx, repository, now, cause, activeErr)
			}
			if !active {
				return PollResult{}, errors.Join(cause, store.ErrNeedsAttention)
			}
			if consumeErr := p.Store.ConsumeGitHubPollBootstrapEligibility(persistenceCtx, repository, now); consumeErr != nil {
				return PollResult{}, p.terminalFailure(persistenceCtx, repository, now, cause, consumeErr)
			}
		}
		return PollResult{}, p.finishExhaustedFailure(persistenceCtx, repository, now, cause)
	}
	return PollResult{}, cause
}

func (p Poller) finishExhaustedFailure(ctx context.Context, repository string, now time.Time, cause error) error {
	return p.terminalFailure(ctx, repository, now, cause)
}

func isPollCredentialFailure(err error) bool {
	if errors.Is(err, delivery.ErrGatewayCredentialRejected) {
		return true
	}
	var failure interface{ AuthenticationFailure() bool }
	return errors.As(err, &failure) && failure.AuthenticationFailure()
}

func (p Poller) terminalFailure(ctx context.Context, repository string, now time.Time, causes ...error) error {
	persistenceCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	result := errors.Join(append(causes, store.ErrNeedsAttention)...)
	if provenanceErr := p.Store.MarkGitHubPollFailureUnrecoverable(persistenceCtx, repository, now); provenanceErr != nil {
		result = errors.Join(result, provenanceErr)
	}
	if attentionErr := p.Store.MarkRepositoryNeedsAttention(persistenceCtx, repository, now); attentionErr != nil {
		return errors.Join(result, attentionErr)
	}
	active, activeErr := p.Store.HasActiveDeliveryPlan(persistenceCtx, repository)
	if activeErr != nil {
		return errors.Join(result, activeErr)
	}
	if active {
		if inboxErr := p.projectWorkflowInbox(persistenceCtx, repository); inboxErr != nil {
			return errors.Join(result, inboxErr)
		}
	}
	return result
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
	reconciler := DeliveredReconciler{Store: p.Store, Client: p.Client}
	for _, delivery := range deliveries {
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
				claim, prompt, claimErr := p.Store.ClaimQueuedReviewRevision(ctx, delivery.VersionID, delivery.IssueID, 30*time.Minute, now, p.maxParallelRuns(), p.MaxWorkerAttempts)
				if claimErr == nil {
					if err := p.LaunchReview(ctx, claim, prompt); err != nil {
						return PollResult{}, err
					}
				} else if !errors.Is(claimErr, store.ErrNotReady) && !errors.Is(claimErr, store.ErrNotFound) {
					return PollResult{}, claimErr
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

func (p Poller) maxParallelRuns() int {
	if p.MaxParallelRuns > 0 {
		return p.MaxParallelRuns
	}
	return 1
}

func (p Poller) routeInboxAnswers(ctx context.Context, repository string) error {
	questions, err := p.Store.OpenWorkflowQuestions(ctx, repository, 0)
	if err != nil {
		return err
	}
	questionIDs := make([]string, 0, len(questions))
	for _, question := range questions {
		questionIDs = append(questionIDs, question.ID)
	}
	answers, err := p.Client.WorkflowInboxAnswers(ctx, repository, questionIDs)
	if err != nil {
		return err
	}
	for _, question := range questions {
		if answer, ok := answers[question.ID]; ok {
			if err := p.Store.AnswerWorkflowQuestion(ctx, repository, question.ID, answer, p.now()); err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
	}
	return nil
}

func (p Poller) projectWorkflowInbox(ctx context.Context, repository string) error {
	if p.InboxProjector == nil {
		return nil
	}
	questions, err := p.Store.OpenWorkflowQuestions(ctx, repository, 0)
	if err != nil {
		return err
	}
	projected := make([]plan.WorkflowQuestion, 0, len(questions))
	for _, question := range questions {
		projected = append(projected, workflowQuestionProjection(question))
	}
	return p.InboxProjector.ProjectWorkflowInbox(ctx, repository, projected)
}

func (p Poller) inboxProjectionFailureKind(ctx context.Context, repository string, cause error) (store.GitHubPollFailureKind, error) {
	var gatewayError *delivery.HTTPError
	if !errors.As(cause, &gatewayError) || gatewayError.StatusCode != 409 || gatewayError.Code != delivery.ErrorCodeNoActiveDeliveryPlan {
		return "", nil
	}
	projecting, err := p.Store.HasProjectingDeliveryPlan(ctx, repository)
	if err != nil {
		return "", err
	}
	if !projecting {
		return "", nil
	}
	return store.GitHubPollFailurePreActivationInboxConflict, nil
}

func workflowQuestionProjection(question store.WorkflowQuestion) plan.WorkflowQuestion {
	return plan.WorkflowQuestion{ID: question.ID, Prompt: question.Prompt, Repository: question.Repository, PlanNumber: question.RootNumber, TicketNumber: question.TicketNumber, PullRequest: question.PullRequest, Commit: question.Commit, Finding: question.Kind, Diagnostics: question.Diagnostics, Evidence: question.Evidence}
}
