package github

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
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
type BootstrapControlResult struct {
	AttemptedPlanVersionID       string
	AttemptedPlanAlreadyComplete bool
}
type BootstrapControlPass func(context.Context, bool) (BootstrapControlResult, error)

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

type retryAtFailure interface {
	RetryAtTime() time.Time
}

var ErrLocalPollStore = errors.New("local GitHub poll persistence is unavailable")

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

func isRemotePollStoreError(err error) bool {
	var gatewayError *delivery.HTTPError
	return errors.Is(err, delivery.ErrGatewayStore) || errors.As(err, &gatewayError) && gatewayError.Code == delivery.ErrorCodeRetryableStore
}

func isLocalPollStoreError(err error) bool {
	if errors.Is(err, ErrLocalPollStore) {
		return true
	}
	if store.IsDatabaseError(err) {
		return true
	}
	if isRemotePollStoreError(err) {
		return false
	}
	var storeErr pollStoreError
	var markedStoreErr interface{ PollStoreFailure() bool }
	return errors.As(err, &storeErr) ||
		errors.As(err, &markedStoreErr) && markedStoreErr.PollStoreFailure()
}

func ClassifyPollError(err error) error {
	if err != nil && isLocalPollStoreError(err) && !errors.Is(err, ErrLocalPollStore) {
		return errors.Join(ErrLocalPollStore, err)
	}
	return err
}

func isPollStoreError(err error) bool {
	return isLocalPollStoreError(err) || isRemotePollStoreError(err)
}

func pollRetryAt(err error) time.Time {
	var failure retryAtFailure
	if errors.As(err, &failure) {
		return failure.RetryAtTime()
	}
	return time.Time{}
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
	attemptedPlanVersionIDs, attemptedErr := p.Store.ActiveDeliveryPlanVersions(leaseCtx, repository)
	if attemptedErr != nil {
		return PollResult{}, errors.Join(cause, attemptedErr, release())
	}
	recoveryPlanVersionID := ""
	if len(attemptedPlanVersionIDs) == 1 {
		recoveryPlanVersionID = attemptedPlanVersionIDs[0]
	} else if len(attemptedPlanVersionIDs) == 0 {
		cursor, cursorErr := p.Store.GitHubPollCursor(leaseCtx, repository)
		if cursorErr != nil && !errors.Is(cursorErr, store.ErrNotFound) {
			return PollResult{}, errors.Join(cause, cursorErr, release())
		}
		if cursorErr == nil && cursor.RecoveryPlanVersionID != "" {
			recoveryPlanVersionID = cursor.RecoveryPlanVersionID
			attemptedPlanVersionIDs = []string{recoveryPlanVersionID}
		}
	}
	result, recordErr := p.recordFailureWithKindForPlans(leaseCtx, repository, p.now(), "", recoveryPlanVersionID, attemptedPlanVersionIDs, cause)
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

// PrepareAdmission resolves exhausted, version-bound bootstrap recovery while
// the repository poll lease is held. This prevents a later credential or
// GitHub admission error from being charged to a plan that has already become
// active, completed, or been replaced.
func (p Poller) PrepareAdmission(ctx context.Context, repository string) error {
	if p.Store == nil {
		return fmt.Errorf("GitHub poller store is unavailable")
	}
	leaseToken, ok := p.pollLeaseToken(ctx, repository)
	if !ok {
		return store.ErrFencingConflict
	}
	cursor, err := p.Store.GitHubPollCursor(ctx, repository)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if !cursor.HasBootstrapRecoveryCandidate(p.maxFailures()) {
		return nil
	}
	_, err = p.Store.ResolveGitHubPollBootstrapRecoveryLeased(ctx, repository, p.maxFailures(), p.now(), leaseToken, p.now())
	return err
}

// RecordAdmissionFailure preserves an exhausted recovery-bound cursor while
// its exact projecting plan still owns the recovery claim. Credential
// rejection remains exceptional because it must pause all Gateway writes.
func (p Poller) RecordAdmissionFailure(ctx context.Context, repository string, cause error) (PollResult, error) {
	leaseToken, leased := p.pollLeaseToken(ctx, repository)
	if !leased {
		leaseCtx, release, err := p.AcquireLease(ctx, repository)
		if err != nil {
			return PollResult{}, errors.Join(cause, err)
		}
		result, recordErr := p.RecordAdmissionFailure(leaseCtx, repository, cause)
		return result, errors.Join(recordErr, release())
	}
	if isLocalPollStoreError(cause) {
		return PollResult{}, ClassifyPollError(cause)
	}
	if isPollCredentialFailure(cause) {
		return p.recordFailure(ctx, repository, p.now(), cause)
	}
	cursor, err := p.Store.GitHubPollCursor(ctx, repository)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return PollResult{}, errors.Join(cause, err)
	}
	if err == nil && cursor.HasBootstrapRecoveryCandidate(p.maxFailures()) {
		if retryAt := pollRetryAt(cause); retryAt.After(p.now()) {
			updated, deferErr := p.Store.DeferGitHubPollBootstrapRecoveryLeased(ctx, repository, cursor.RecoveryPlanVersionID, retryAt, p.now(), leaseToken, p.now())
			if deferErr != nil {
				return PollResult{}, errors.Join(cause, deferErr)
			}
			if updated.ConsecutiveFailures > p.bootstrapRecoveryFailureLimit() {
				return PollResult{}, p.finishExhaustedFailure(ctx, repository, p.now(), cause)
			}
			return PollResult{}, cause
		}
		return PollResult{}, p.finishExhaustedFailure(ctx, repository, p.now(), cause)
	}
	return p.recordFailure(ctx, repository, p.now(), cause)
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
		control = func(ctx context.Context, _ bool) (BootstrapControlResult, error) {
			return BootstrapControlResult{}, before(ctx)
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
	return result, ClassifyPollError(pollErr)
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
			eligible, eligibilityErr := p.Store.HasWorkflowInboxDeliveryPlan(ctx, repository)
			if eligibilityErr != nil {
				return PollResult{}, errors.Join(pausedErr, eligibilityErr)
			}
			if eligible {
				if err := p.renewPollLease(ctx, repository); err != nil {
					return PollResult{}, errors.Join(pausedErr, err)
				}
				if err := p.projectWorkflowInbox(ctx, repository); err != nil {
					return PollResult{}, errors.Join(pausedErr, err)
				}
				if err := p.routeInboxAnswers(ctx, repository); err != nil {
					return PollResult{}, errors.Join(pausedErr, err)
				}
				refreshed, refreshErr := p.Store.GitHubPollCursor(ctx, repository)
				if refreshErr != nil {
					return PollResult{}, errors.Join(pausedErr, refreshErr)
				}
				if !refreshed.NeedsAttention() {
					cursor = refreshed
				} else {
					return PollResult{}, pausedErr
				}
			} else {
				return PollResult{}, pausedErr
			}
		}
		if cursor.ConsecutiveFailures >= p.maxFailures() {
			if !cursor.HasBootstrapRecoveryCandidate(p.maxFailures()) {
				return PollResult{}, p.finishExhaustedFailure(ctx, repository, now, fmt.Errorf("GitHub poll bootstrap recovery is unavailable"))
			}
			disposition, resolveErr := p.Store.ResolveGitHubPollBootstrapRecoveryLeased(ctx, repository, p.maxFailures(), now, leaseToken, p.now())
			if resolveErr != nil {
				return PollResult{}, resolveErr
			}
			switch disposition {
			case store.GitHubPollBootstrapRecoveryActive:
				bootstrapped = true
			case store.GitHubPollBootstrapRecoveryProjecting:
				recovered, recoveryErr := p.resumeClaimedBootstrapRecovery(ctx, repository, cursor, bootstrap, now, leaseToken)
				if recoveryErr != nil {
					return PollResult{}, recoveryErr
				}
				if recovered {
					bootstrapped = true
				}
			case store.GitHubPollBootstrapRecoveryStale:
			default:
				return PollResult{}, p.finishExhaustedFailure(ctx, repository, now, fmt.Errorf("GitHub poll bootstrap provenance is no longer current"))
			}
		}
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return PollResult{}, err
	}
	activeBeforeControlVersionIDs, err := p.Store.ActiveDeliveryPlanVersions(ctx, repository)
	if err != nil {
		return PollResult{}, err
	}
	if len(activeBeforeControlVersionIDs) > 0 {
		if err := p.renewPollLease(ctx, repository); err != nil {
			return PollResult{}, err
		}
		if err := p.routeInboxAnswers(ctx, repository); err != nil {
			if isLocalPollStoreError(err) {
				return PollResult{}, err
			}
			return p.recordFailureForPlans(ctx, repository, now, activeBeforeControlVersionIDs, err)
		}
	}
	full := cursorMissing || cursor.LastFullReconcileAt.IsZero() || now.Sub(cursor.LastFullReconcileAt) >= p.fullReconcileInterval()
	controlResult := BootstrapControlResult{}
	if before != nil {
		if err := p.renewPollLease(ctx, repository); err != nil {
			return PollResult{}, err
		}
		controlResult, err = before(ctx, bootstrapped)
		if err != nil {
			return p.recordBootstrapFailure(ctx, repository, now, controlResult.AttemptedPlanVersionID, controlResult.AttemptedPlanAlreadyComplete, err)
		}
	}
	if err := p.renewPollLease(ctx, repository); err != nil {
		return PollResult{}, err
	}
	attemptedActiveVersionIDs, err := p.Store.ActiveDeliveryPlanVersions(ctx, repository)
	if err != nil {
		return PollResult{}, err
	}
	recoveryCandidateVersionID := controlResult.AttemptedPlanVersionID
	if recoveryCandidateVersionID == "" && cursor.FailureKind == store.GitHubPollFailurePreActivationInboxConflict &&
		(cursor.RecoveryState == store.GitHubPollRecoveryAvailable || cursor.RecoveryState == store.GitHubPollRecoveryClaimed) {
		recoveryCandidateVersionID = cursor.RecoveryPlanVersionID
	}
	if len(attemptedActiveVersionIDs) == 0 && recoveryCandidateVersionID == "" {
		if err := p.Store.RecordGitHubPollSuccessLeased(ctx, repository, now, full, leaseToken, p.now()); err != nil {
			return PollResult{}, err
		}
		return PollResult{}, nil
	}
	if err := p.projectWorkflowInbox(ctx, repository); err != nil {
		if isLocalPollStoreError(err) {
			return PollResult{}, err
		}
		failureKind, recoveryPlanVersionID, ignoreFailure, classificationErr := p.inboxProjectionFailureKind(ctx, repository, attemptedActiveVersionIDs, recoveryCandidateVersionID, err)
		if classificationErr != nil {
			return PollResult{}, errors.Join(err, classificationErr)
		}
		if ignoreFailure {
			if successErr := p.Store.RecordGitHubPollSuccessLeased(ctx, repository, now, full, leaseToken, p.now()); successErr != nil {
				return PollResult{}, successErr
			}
			return PollResult{}, nil
		}
		return p.recordFailureWithKindForPlans(ctx, repository, now, failureKind, recoveryPlanVersionID, attemptedActiveVersionIDs, err)
	}
	if err := p.renewPollLease(ctx, repository); err != nil {
		return PollResult{}, err
	}
	result, err := p.poll(ctx, repository, now, cursor.LastSuccessAt, full)
	if err != nil {
		if isLocalPollStoreError(err) {
			return PollResult{}, err
		}
		return p.recordFailureForPlans(ctx, repository, now, attemptedActiveVersionIDs, err)
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
	projecting, err := p.Store.IsProjectingDeliveryPlanVersion(ctx, repository, cursor.RecoveryPlanVersionID)
	if err != nil {
		return false, err
	}
	if bootstrap == nil || !projecting {
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
		if _, resolveErr := p.Store.ResolveGitHubPollBootstrapRecoveryLeased(ctx, repository, p.maxFailures(), now, leaseToken, p.now()); resolveErr != nil {
			return false, errors.Join(err, resolveErr)
		}
		_, failureErr := p.recordBootstrapFailure(ctx, repository, now, cursor.RecoveryPlanVersionID, false, err)
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
	return p.recordFailureForPlans(ctx, repository, now, nil, cause)
}

func (p Poller) recordFailureForPlans(ctx context.Context, repository string, now time.Time, attemptedPlanVersionIDs []string, cause error) (PollResult, error) {
	return p.recordFailureWithKindForPlans(ctx, repository, now, "", "", attemptedPlanVersionIDs, cause)
}

func (p Poller) recordBootstrapFailure(ctx context.Context, repository string, now time.Time, attemptedPlanVersionID string, attemptedPlanAlreadyComplete bool, cause error) (PollResult, error) {
	if attemptedPlanAlreadyComplete {
		attemptedPlanVersionID = ""
	}
	if attemptedPlanVersionID == "" || isLocalPollStoreError(cause) {
		return p.recordFailure(ctx, repository, now, cause)
	}
	projecting, err := p.Store.IsProjectingDeliveryPlanVersion(ctx, repository, attemptedPlanVersionID)
	if err != nil {
		return PollResult{}, errors.Join(cause, err)
	}
	if !projecting {
		return p.recordFailureWithKindForPlans(ctx, repository, now, "", attemptedPlanVersionID, []string{attemptedPlanVersionID}, cause)
	}
	return p.recordFailureWithKindForPlans(ctx, repository, now, store.GitHubPollFailurePreActivationInboxConflict, attemptedPlanVersionID, []string{attemptedPlanVersionID}, cause)
}

func (p Poller) recordFailureWithKind(ctx context.Context, repository string, now time.Time, failureKind store.GitHubPollFailureKind, recoveryPlanVersionID string, cause error) (PollResult, error) {
	return p.recordFailureWithKindForPlans(ctx, repository, now, failureKind, recoveryPlanVersionID, nil, cause)
}

func (p Poller) recordFailureWithKindForPlans(ctx context.Context, repository string, now time.Time, failureKind store.GitHubPollFailureKind, recoveryPlanVersionID string, attemptedPlanVersionIDs []string, cause error) (PollResult, error) {
	if isLocalPollStoreError(cause) {
		return PollResult{}, ClassifyPollError(cause)
	}
	persistenceCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	leaseToken, leased := p.pollLeaseToken(ctx, repository)
	if !leased {
		return PollResult{}, errors.Join(cause, store.ErrFencingConflict)
	}
	if isPollCredentialFailure(cause) {
		pauseErr := p.Store.PauseGatewayWritesForGitHubPollCredential(persistenceCtx, repository, leaseToken, "Gateway Credential is unavailable; replace and verify it to resume writes", now, p.now())
		return PollResult{}, errors.Join(cause, pauseErr)
	}
	var existingCursor store.GitHubPollCursor
	var existingCursorErr error
	conflictingBootstrapFailureHistory := false
	if failureKind == store.GitHubPollFailurePreActivationInboxConflict && recoveryPlanVersionID != "" {
		existingCursor, existingCursorErr = p.Store.GitHubPollCursor(persistenceCtx, repository)
		if existingCursorErr != nil && !errors.Is(existingCursorErr, store.ErrNotFound) {
			return PollResult{}, errors.Join(cause, existingCursorErr)
		}
		continuingBootstrapRecovery := existingCursorErr == nil && existingCursor.HasBootstrapRecoveryCandidate(1) && existingCursor.RecoveryPlanVersionID == recoveryPlanVersionID
		conflictingBootstrapFailureHistory = existingCursorErr == nil && existingCursor.ConsecutiveFailures > 0 && !continuingBootstrapRecovery
	}
	if retryAt := pollRetryAt(cause); retryAt.After(now) {
		var updated store.GitHubPollCursor
		var err error
		if failureKind == store.GitHubPollFailurePreActivationInboxConflict && recoveryPlanVersionID != "" {
			if existingCursorErr == nil && existingCursor.HasBootstrapRecoveryCandidate(p.maxFailures()) && existingCursor.RecoveryPlanVersionID == recoveryPlanVersionID {
				updated, err = p.Store.DeferGitHubPollBootstrapRecoveryForPlanAttemptsLeased(persistenceCtx, repository, recoveryPlanVersionID, attemptedPlanVersionIDs, retryAt, now, leaseToken, p.now())
				if err != nil {
					return PollResult{}, errors.Join(cause, err)
				}
				if updated.ConsecutiveFailures > p.bootstrapRecoveryFailureLimit() {
					return PollResult{}, p.terminalFailureForPlanAttempts(persistenceCtx, repository, recoveryPlanVersionID, attemptedPlanVersionIDs, now, cause)
				}
				return PollResult{}, cause
			}
			updated, err = p.Store.DeferGitHubPollBootstrapRecoveryForPlanAttemptsLeased(persistenceCtx, repository, recoveryPlanVersionID, attemptedPlanVersionIDs, retryAt, now, leaseToken, p.now())
			if err != nil {
				return PollResult{}, errors.Join(cause, err)
			}
		} else {
			updated, err = p.Store.DeferGitHubPollWithCursorForPlanAttemptsLeased(persistenceCtx, repository, attemptedPlanVersionIDs, retryAt, now, leaseToken, p.now())
			if err != nil {
				return PollResult{}, errors.Join(cause, err)
			}
		}
		if updated.ConsecutiveFailures >= p.maxFailures() {
			return PollResult{}, p.terminalFailureForPlanAttempts(persistenceCtx, repository, recoveryPlanVersionID, attemptedPlanVersionIDs, now, cause)
		}
		return PollResult{}, cause
	}
	var updated store.GitHubPollCursor
	var recordErr error
	updated, recordErr = p.Store.AdvanceGitHubPollFailureForPlanAttemptsLeased(persistenceCtx, repository, now, failureKind, recoveryPlanVersionID, attemptedPlanVersionIDs, leaseToken, p.now())
	if recordErr != nil {
		return PollResult{}, errors.Join(cause, recordErr)
	}
	if updated.ConsecutiveFailures >= p.maxFailures() {
		if updated.FailureKind == store.GitHubPollFailurePreActivationInboxConflict && updated.RecoveryState == store.GitHubPollRecoveryAvailable {
			if conflictingBootstrapFailureHistory {
				return PollResult{}, p.terminalFailureForPlanAttempts(persistenceCtx, repository, recoveryPlanVersionID, attemptedPlanVersionIDs, now, cause)
			}
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
		return PollResult{}, p.terminalFailureForPlanAttempts(persistenceCtx, repository, recoveryPlanVersionID, attemptedPlanVersionIDs, now, cause)
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
	return p.terminalFailureForPlan(ctx, repository, "", now, causes...)
}

func (p Poller) terminalFailureForPlan(ctx context.Context, repository, recoveryPlanVersionID string, now time.Time, causes ...error) error {
	return p.terminalFailureForPlanAttempts(ctx, repository, recoveryPlanVersionID, nil, now, causes...)
}

func (p Poller) terminalFailureForPlanAttempts(ctx context.Context, repository, recoveryPlanVersionID string, attemptedPlanVersionIDs []string, now time.Time, causes ...error) error {
	persistenceCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	result := errors.Join(causes...)
	leaseToken, leased := p.pollLeaseToken(ctx, repository)
	if !leased {
		return errors.Join(result, store.ErrFencingConflict)
	}
	terminalized, attentionErr := p.Store.ResolveGitHubPollTerminalFailureForPlanAttemptsLeased(persistenceCtx, repository, recoveryPlanVersionID, attemptedPlanVersionIDs, now, leaseToken, p.now())
	if attentionErr != nil {
		result = errors.Join(result, attentionErr)
		return result
	}
	if !terminalized {
		return nil
	}
	result = errors.Join(result, store.ErrNeedsAttention)
	eligible, eligibilityErr := p.Store.HasWorkflowInboxDeliveryPlan(persistenceCtx, repository)
	if eligibilityErr != nil {
		return errors.Join(result, eligibilityErr)
	}
	if eligible {
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

func (p Poller) bootstrapRecoveryFailureLimit() int {
	return p.maxFailures() * 2
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
	questions, err := p.Store.WorkflowInboxQuestions(ctx, repository)
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
			leaseToken, leased := p.pollLeaseToken(ctx, repository)
			if !leased {
				return wrapPollStoreError(store.ErrFencingConflict)
			}
			var err error
			if question.Kind == "inbox_delivery_recovery" {
				_, err = p.Store.RecoverUncertainInboxDeliveryQuestionLeased(ctx, repository, question.ID, answer, p.now(), leaseToken, p.now())
			} else {
				_, err = p.Store.AnswerWorkflowQuestionAndQueueInboxProjectionLeased(ctx, repository, question.ID, answer, p.now(), leaseToken, p.now())
			}
			if err != nil && !errors.Is(err, store.ErrNotFound) {
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
	questions, err := p.Store.WorkflowInboxQuestions(ctx, repository)
	if err != nil {
		return wrapPollStoreError(err)
	}
	return p.InboxProjector.ProjectWorkflowInbox(ctx, repository, store.WorkflowQuestionProjections(questions))
}

func (p Poller) inboxProjectionFailureKind(ctx context.Context, repository string, attemptedActiveVersionIDs []string, recoveryCandidateVersionID string, cause error) (store.GitHubPollFailureKind, string, bool, error) {
	var gatewayError *delivery.HTTPError
	if !errors.As(cause, &gatewayError) || gatewayError.StatusCode != 409 || gatewayError.Code != delivery.ErrorCodeNoActiveDeliveryPlan {
		return "", "", false, nil
	}
	activeVersionIDs, err := p.Store.ActiveDeliveryPlanVersions(ctx, repository)
	if err != nil {
		return "", "", false, err
	}
	if len(attemptedActiveVersionIDs) > 0 {
		return "", "", !slices.Equal(attemptedActiveVersionIDs, activeVersionIDs), nil
	}
	if recoveryCandidateVersionID == "" {
		return "", "", true, nil
	}
	projecting, err := p.Store.IsProjectingDeliveryPlanVersion(ctx, repository, recoveryCandidateVersionID)
	if err != nil {
		return "", "", false, err
	}
	if !projecting {
		return "", "", true, nil
	}
	return store.GitHubPollFailurePreActivationInboxConflict, recoveryCandidateVersionID, false, nil
}
