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

type pollStoreError struct {
	err error
}

func (e pollStoreError) Error() string {
	return e.err.Error()
}

func (e pollStoreError) Unwrap() error {
	return e.err
}

func wrapPollStoreError(err error) error {
	if err == nil {
		return nil
	}
	return pollStoreError{err: err}
}

func isPollStoreError(err error) bool {
	var storeErr pollStoreError
	return errors.As(err, &storeErr) || store.IsDatabaseError(err)
}

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
	leaseCtx, release, err := p.AcquireLease(ctx, repository)
	if err != nil {
		return PollResult{}, errors.Join(cause, err)
	}
	result, recordErr := p.recordFailure(leaseCtx, repository, p.now(), cause)
	return result, errors.Join(recordErr, release())
}

func (p Poller) RecordTerminalFailure(ctx context.Context, repository string, cause error) (PollResult, error) {
	if p.Store == nil {
		return PollResult{}, errors.Join(cause, fmt.Errorf("GitHub poller store is unavailable"), store.ErrNeedsAttention)
	}
	if err := ValidateRepository(repository); err != nil {
		return PollResult{}, errors.Join(cause, err, store.ErrNeedsAttention)
	}
	leaseCtx, release, err := p.AcquireLease(ctx, repository)
	if err != nil {
		return PollResult{}, errors.Join(cause, err, store.ErrNeedsAttention)
	}
	terminalErr := p.terminalFailure(leaseCtx, repository, p.now(), cause)
	return PollResult{}, errors.Join(terminalErr, release())
}

func (p Poller) Ready(ctx context.Context, repository string) error {
	if p.Store == nil {
		return fmt.Errorf("GitHub poller store is unavailable")
	}
	if err := ValidateRepository(repository); err != nil {
		return err
	}
	if _, ok := p.pollLeaseToken(ctx, repository); ok {
		if err := p.renewPollLease(ctx, repository); err != nil {
			return err
		}
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
	leaseCtx, release, err := p.AcquireLease(ctx, repository)
	if err != nil {
		return err
	}
	persistenceCtx, cancel := context.WithTimeout(context.WithoutCancel(leaseCtx), 30*time.Second)
	defer cancel()
	token, ok := p.pollLeaseToken(leaseCtx, repository)
	if !ok {
		return errors.Join(store.ErrFencingConflict, release())
	}
	now := p.now()
	return errors.Join(p.Store.ConsumeGitHubPollBootstrapEligibilityLeased(persistenceCtx, repository, token, now, p.now()), release())
}

func (p Poller) pollLeaseToken(ctx context.Context, repository string) (string, bool) {
	lease, ok := ctx.Value(githubPollLeaseContextKey{}).(githubPollLease)
	return lease.token, ok && lease.repository == repository && lease.token != ""
}

func (p Poller) renewPollLease(ctx context.Context, repository string) error {
	token, ok := p.pollLeaseToken(ctx, repository)
	if !ok {
		return store.ErrFencingConflict
	}
	return p.Store.RenewGitHubPollLease(ctx, repository, token, p.now(), githubPollLeaseTTL)
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
	leaseToken, ok := p.pollLeaseToken(ctx, repository)
	if !ok {
		return PollResult{}, store.ErrFencingConflict
	}
	if err := p.readyAt(ctx, repository, now); err != nil {
		if errors.Is(err, store.ErrNotReady) {
			return PollResult{}, err
		}
		return PollResult{}, err
	}
	bootstrapped := false
	cursor, err := p.Store.GitHubPollCursor(ctx, repository)
	cursorMissing := errors.Is(err, store.ErrNotFound)
	if err == nil {
		if cursor.NeedsAttention() {
			pausedErr := errors.Join(fmt.Errorf("GitHub polling is paused pending human recovery"), store.ErrNeedsAttention)
			active, activeErr := p.Store.HasActiveDeliveryPlan(ctx, repository)
			if activeErr != nil {
				return PollResult{}, errors.Join(pausedErr, activeErr)
			}
			if active {
				if err := p.renewPollLease(ctx, repository); err != nil {
					return PollResult{}, errors.Join(pausedErr, err)
				}
				if err := p.projectWorkflowInbox(ctx, repository); err != nil {
					return PollResult{}, errors.Join(pausedErr, err)
				}
			}
			return PollResult{}, pausedErr
		}
		if cursor.ConsecutiveFailures >= p.maxFailures() {
			if cursor.FailureKind != store.GitHubPollFailurePreActivationInboxConflict || cursor.RecoveryState != store.GitHubPollRecoveryAvailable && cursor.RecoveryState != store.GitHubPollRecoveryClaimed {
				return PollResult{}, p.finishExhaustedFailure(ctx, repository, now, fmt.Errorf("GitHub poll bootstrap recovery is unavailable"))
			}
			disposition, resolveErr := p.Store.ResolveGitHubPollBootstrapRecoveryLeased(ctx, repository, p.maxFailures(), now, leaseToken, p.now())
			if resolveErr != nil {
				return PollResult{}, resolveErr
			}
			switch disposition {
			case store.GitHubPollBootstrapRecoveryActive:
				bootstrapped = true
				cursor.ConsecutiveFailures = 0
				cursor.FailureKind = ""
				cursor.RecoveryState = store.GitHubPollRecoveryCompleted
				cursor.NextAttemptAt = now
			case store.GitHubPollBootstrapRecoveryProjecting:
				cursor.RecoveryState = store.GitHubPollRecoveryClaimed
				recovered, recoveryErr := p.resumeClaimedBootstrapRecovery(ctx, repository, cursor, bootstrap, now, leaseToken)
				if recoveryErr != nil {
					return PollResult{}, recoveryErr
				}
				if recovered {
					bootstrapped = true
					cursor.ConsecutiveFailures = 0
					cursor.FailureKind = ""
					cursor.RecoveryState = store.GitHubPollRecoveryCompleted
					cursor.NextAttemptAt = now
				} else {
					cursor.ConsecutiveFailures = 0
					cursor.FailureKind = store.GitHubPollFailureRetryable
					cursor.RecoveryState = store.GitHubPollRecoveryConsumed
					cursor.RecoveryPlanVersionID = ""
					cursor.NextAttemptAt = now
				}
			case store.GitHubPollBootstrapRecoveryStale:
				cursor.ConsecutiveFailures = 0
				cursor.FailureKind = store.GitHubPollFailureRetryable
				cursor.RecoveryState = store.GitHubPollRecoveryConsumed
				cursor.RecoveryPlanVersionID = ""
				cursor.NextAttemptAt = now
			default:
				return PollResult{}, p.finishExhaustedFailure(ctx, repository, now, fmt.Errorf("GitHub poll bootstrap provenance is no longer current"))
			}
		}
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return PollResult{}, err
	}
	activeBeforeControl, err := p.Store.HasActiveDeliveryPlan(ctx, repository)
	if err != nil {
		return PollResult{}, err
	}
	if activeBeforeControl {
		if err := p.renewPollLease(ctx, repository); err != nil {
			return PollResult{}, err
		}
		if err := p.routeInboxAnswers(ctx, repository); err != nil {
			if isPollStoreError(err) {
				return PollResult{}, err
			}
			return p.recordFailure(ctx, repository, now, err)
		}
	}
	full := cursorMissing || cursor.LastFullReconcileAt.IsZero() || now.Sub(cursor.LastFullReconcileAt) >= p.fullReconcileInterval()
	if before != nil {
		if err := p.renewPollLease(ctx, repository); err != nil {
			return PollResult{}, err
		}
		if err := before(ctx, bootstrapped); err != nil {
			return p.recordFailure(ctx, repository, now, err)
		}
	}
	if err := p.renewPollLease(ctx, repository); err != nil {
		return PollResult{}, err
	}
	active, err := p.Store.HasActiveDeliveryPlan(ctx, repository)
	if err != nil {
		return PollResult{}, err
	}
	recoveryCandidateVersionID, singleProjectingPlan, err := p.Store.ProjectingDeliveryPlanVersion(ctx, repository)
	if err != nil {
		return PollResult{}, err
	}
	if !singleProjectingPlan {
		recoveryCandidateVersionID = ""
	}
	if !active && !singleProjectingPlan {
		if err := p.Store.RecordGitHubPollSuccessLeased(ctx, repository, now, full, leaseToken, p.now()); err != nil {
			return PollResult{}, err
		}
		return PollResult{}, nil
	}
	if err := p.projectWorkflowInbox(ctx, repository); err != nil {
		if isPollStoreError(err) {
			return PollResult{}, err
		}
		failureKind, recoveryPlanVersionID, classificationErr := p.inboxProjectionFailureKind(ctx, repository, recoveryCandidateVersionID, err)
		if classificationErr != nil {
			return PollResult{}, errors.Join(err, classificationErr)
		}
		return p.recordFailureWithKind(ctx, repository, now, failureKind, recoveryPlanVersionID, err)
	}
	if err := p.renewPollLease(ctx, repository); err != nil {
		return PollResult{}, err
	}
	result, err := p.poll(ctx, repository, now, cursor.LastSuccessAt, full)
	if err != nil {
		if isPollStoreError(err) {
			return PollResult{}, err
		}
		return p.recordFailure(ctx, repository, now, err)
	}
	if err := p.Store.RecordGitHubPollSuccessLeased(ctx, repository, now, full, leaseToken, p.now()); err != nil {
		return PollResult{}, err
	}
	return result, nil
}

func (p Poller) resumeClaimedBootstrapRecovery(ctx context.Context, repository string, cursor store.GitHubPollCursor, bootstrap ControlPass, now time.Time, leaseToken string) (bool, error) {
	recovered, err := p.Store.RecoverGitHubPollAfterBootstrapLeased(ctx, repository, now, leaseToken, p.now())
	if err != nil {
		return false, err
	}
	if recovered {
		return true, nil
	}
	versionID, projecting, err := p.Store.ProjectingDeliveryPlanVersion(ctx, repository)
	if err != nil {
		return false, err
	}
	if bootstrap == nil || !projecting || versionID != cursor.RecoveryPlanVersionID {
		disposition, resolveErr := p.Store.ResolveGitHubPollBootstrapRecoveryLeased(ctx, repository, p.maxFailures(), now, leaseToken, p.now())
		if resolveErr != nil {
			return false, resolveErr
		}
		if disposition == store.GitHubPollBootstrapRecoveryActive {
			return true, nil
		}
		if disposition == store.GitHubPollBootstrapRecoveryStale {
			return false, nil
		}
		return false, p.finishExhaustedFailure(ctx, repository, now, fmt.Errorf("GitHub poll bootstrap recovery claim is not resumable"))
	}
	if err := p.renewPollLease(ctx, repository); err != nil {
		return false, err
	}
	if err := bootstrap(ctx); err != nil {
		_, failureErr := p.recordFailure(ctx, repository, now, err)
		return false, failureErr
	}
	recovered, err = p.Store.RecoverGitHubPollAfterBootstrapLeased(ctx, repository, now, leaseToken, p.now())
	if err != nil {
		return false, err
	}
	if recovered {
		return true, nil
	}
	disposition, resolveErr := p.Store.ResolveGitHubPollBootstrapRecoveryLeased(ctx, repository, p.maxFailures(), now, leaseToken, p.now())
	if resolveErr != nil {
		return false, resolveErr
	}
	if disposition == store.GitHubPollBootstrapRecoveryActive {
		return true, nil
	}
	if disposition == store.GitHubPollBootstrapRecoveryStale {
		return false, nil
	}
	return false, p.terminalFailure(ctx, repository, now, fmt.Errorf("plan bootstrap did not activate the recovery-bound delivery plan"))
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
	return p.recordFailureWithKind(ctx, repository, now, "", "", cause)
}

func (p Poller) recordFailureWithKind(ctx context.Context, repository string, now time.Time, failureKind store.GitHubPollFailureKind, recoveryPlanVersionID string, cause error) (PollResult, error) {
	if isPollStoreError(cause) {
		return PollResult{}, cause
	}
	persistenceCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	leaseToken, leased := p.pollLeaseToken(ctx, repository)
	if !leased {
		return PollResult{}, errors.Join(cause, store.ErrFencingConflict)
	}
	if isPollCredentialFailure(cause) {
		consumeErr := p.Store.PauseGitHubPollForCredential(persistenceCtx, repository, leaseToken, now, p.now())
		var pauseErr error
		if consumeErr == nil {
			pauseErr = p.Store.PauseGatewayWritesForGitHubPoll(persistenceCtx, repository, leaseToken, "Gateway Credential is unavailable; replace and verify it to resume writes", now, p.now())
		}
		return PollResult{}, errors.Join(cause, consumeErr, pauseErr)
	}
	var githubError *apiError
	if errors.As(cause, &githubError) && githubError.RetryAt.After(now) {
		var updated store.GitHubPollCursor
		var err error
		updated, err = p.Store.DeferGitHubPollWithCursorLeased(persistenceCtx, repository, githubError.RetryAt, now, leaseToken, p.now())
		if err != nil {
			return PollResult{}, errors.Join(cause, err)
		}
		if updated.ConsecutiveFailures >= p.maxFailures() {
			return PollResult{}, p.finishExhaustedFailure(persistenceCtx, repository, now, cause)
		}
		return PollResult{}, cause
	}
	var updated store.GitHubPollCursor
	var recordErr error
	updated, recordErr = p.Store.AdvanceGitHubPollFailureLeased(persistenceCtx, repository, now, failureKind, recoveryPlanVersionID, leaseToken, p.now())
	if recordErr != nil {
		return PollResult{}, errors.Join(cause, recordErr)
	}
	if updated.ConsecutiveFailures >= p.maxFailures() {
		if updated.FailureKind == store.GitHubPollFailurePreActivationInboxConflict && updated.RecoveryState == store.GitHubPollRecoveryAvailable {
			active, activeErr := p.Store.HasActiveDeliveryPlan(persistenceCtx, repository)
			if activeErr != nil {
				return PollResult{}, errors.Join(cause, activeErr)
			}
			if !active {
				return PollResult{}, errors.Join(cause, store.ErrNeedsAttention)
			}
			var consumeErr error
			consumeErr = p.Store.ConsumeGitHubPollBootstrapEligibilityLeased(persistenceCtx, repository, leaseToken, now, p.now())
			if consumeErr != nil {
				return PollResult{}, errors.Join(cause, consumeErr)
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
	result := errors.Join(causes...)
	leaseToken, leased := p.pollLeaseToken(ctx, repository)
	if !leased {
		return errors.Join(result, store.ErrFencingConflict)
	}
	attentionErr := p.Store.MarkGitHubPollFailureUnrecoverableAndRepositoryNeedsAttentionLeased(persistenceCtx, repository, now, leaseToken, p.now())
	if attentionErr != nil {
		result = errors.Join(result, attentionErr)
		return result
	}
	result = errors.Join(result, store.ErrNeedsAttention)
	active, activeErr := p.Store.HasActiveDeliveryPlan(persistenceCtx, repository)
	if activeErr != nil {
		return errors.Join(result, activeErr)
	}
	if active {
		if leased {
			if err := p.Store.RenewGitHubPollLease(persistenceCtx, repository, leaseToken, p.now(), githubPollLeaseTTL); err != nil {
				return errors.Join(result, err)
			}
		}
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
		return PollResult{}, wrapPollStoreError(err)
	}
	result := PollResult{Deliveries: len(deliveries)}
	reconciler := DeliveredReconciler{Store: p.Store, Client: p.Client}
	for _, delivery := range deliveries {
		if err := p.renewPollLease(ctx, repository); err != nil {
			return PollResult{}, err
		}
		terminal, err := reconciler.ReconcileTicket(ctx, delivery)
		if err != nil {
			return PollResult{}, err
		}
		if !terminal {
			if err := p.renewPollLease(ctx, repository); err != nil {
				return PollResult{}, err
			}
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
				return PollResult{}, wrapPollStoreError(err)
			}
			result.Feedback += inserted
			if p.LaunchReview != nil {
				if err := p.renewPollLease(ctx, repository); err != nil {
					return PollResult{}, err
				}
				claim, prompt, claimErr := p.Store.ClaimQueuedReviewRevision(ctx, delivery.VersionID, delivery.IssueID, 30*time.Minute, now, p.maxParallelRuns(), p.MaxWorkerAttempts)
				if claimErr == nil {
					if err := p.LaunchReview(ctx, claim, prompt); err != nil {
						return PollResult{}, err
					}
				} else if !errors.Is(claimErr, store.ErrNotReady) && !errors.Is(claimErr, store.ErrNotFound) {
					return PollResult{}, wrapPollStoreError(claimErr)
				}
			}
		}
		if err := p.renewPollLease(ctx, repository); err != nil {
			return PollResult{}, err
		}
		checks, etag, changed, err := p.Client.PullRequestChecksIfChanged(ctx, repository, delivery.CandidateCommit, delivery.ChecksETag, full)
		if err != nil {
			return PollResult{}, err
		}
		if changed {
			updated, err := p.Store.RecordPullRequestChecks(ctx, delivery.VersionID, delivery.IssueID, checks, now)
			if err != nil {
				return PollResult{}, wrapPollStoreError(err)
			}
			if err := p.Store.RecordPullRequestChecksETag(ctx, delivery.VersionID, delivery.IssueID, etag); err != nil {
				return PollResult{}, wrapPollStoreError(err)
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
		return wrapPollStoreError(err)
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
				return wrapPollStoreError(err)
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
		return wrapPollStoreError(err)
	}
	projected := make([]plan.WorkflowQuestion, 0, len(questions))
	for _, question := range questions {
		projected = append(projected, workflowQuestionProjection(question))
	}
	return p.InboxProjector.ProjectWorkflowInbox(ctx, repository, projected)
}

func (p Poller) inboxProjectionFailureKind(ctx context.Context, repository, candidateVersionID string, cause error) (store.GitHubPollFailureKind, string, error) {
	var gatewayError *delivery.HTTPError
	if !errors.As(cause, &gatewayError) || gatewayError.StatusCode != 409 || gatewayError.Code != delivery.ErrorCodeNoActiveDeliveryPlan {
		return "", "", nil
	}
	if candidateVersionID == "" {
		return "", "", nil
	}
	versionID, projecting, err := p.Store.ProjectingDeliveryPlanVersion(ctx, repository)
	if err != nil {
		return "", "", err
	}
	if !projecting || versionID != candidateVersionID {
		return "", "", nil
	}
	return store.GitHubPollFailurePreActivationInboxConflict, candidateVersionID, nil
}

func workflowQuestionProjection(question store.WorkflowQuestion) plan.WorkflowQuestion {
	return plan.WorkflowQuestion{ID: question.ID, Prompt: question.Prompt, Repository: question.Repository, PlanNumber: question.RootNumber, TicketNumber: question.TicketNumber, PullRequest: question.PullRequest, Commit: question.Commit, Finding: question.Kind, Diagnostics: question.Diagnostics, Evidence: question.Evidence}
}
