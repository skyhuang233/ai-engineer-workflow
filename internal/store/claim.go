package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/skyhuang233/workflow/internal/plan"
)

type ClaimRequest struct {
	VersionID        string
	TicketID         int64
	Owner            string
	MaxParallelRuns  int
	MaxAttempts      int
	LeaseTTL         time.Duration
	Now              time.Time
	ProvisionSession SessionProvisioner

	// fairness fields are populated only by ClaimNextReady. Keeping them
	// private prevents explicit ticket claims from perturbing global turns.
	fairnessRepository  string
	fairnessRootIssueID int64
	fairnessRootNumber  int64
}

type SessionProvisioning struct {
	SessionID      string
	Existing       bool
	WorkspacePath  string
	CodexStatePath string
	CurrentRunID   string
}

type SessionProvisioningResult struct {
	Rollback func() error
}

type SessionProvisioner func(context.Context, SessionProvisioning) (SessionProvisioningResult, error)

var ErrSessionAuthenticationUnavailable = errors.New("Ticket Session Codex authentication cache is unavailable")

type SessionAuthenticationFailure struct {
	DiagnosticsPath string
}

func (e *SessionAuthenticationFailure) Error() string {
	return ErrSessionAuthenticationUnavailable.Error()
}

func (e *SessionAuthenticationFailure) Unwrap() error {
	return ErrSessionAuthenticationUnavailable
}

type sessionAuthenticationTerminalizedError struct {
	cause error
}

func (e *sessionAuthenticationTerminalizedError) Error() string {
	return e.cause.Error()
}

func (e *sessionAuthenticationTerminalizedError) Unwrap() error {
	return e.cause
}

func IsSessionAuthenticationTerminalized(err error) bool {
	var terminalized *sessionAuthenticationTerminalizedError
	return errors.As(err, &terminalized)
}

const DefaultMaxWorkerAttempts = 3

func maxWorkerAttempts(value int) int {
	if value <= 0 {
		return DefaultMaxWorkerAttempts
	}
	return value
}

type TicketClaim struct {
	VersionID           string
	PlanRootNumber      int64
	TicketID            int64
	TicketNumber        int64
	TicketTitle         string
	Owner               string
	SessionID           string
	RunID               string
	Attempt             int
	LeaseToken          string
	LeaseGeneration     int64
	IsolationGeneration int64
	LeaseExpiresAt      time.Time
}

type DeliveryIsolationProof struct {
	target TicketClaim
}

// TicketBody returns the immutable specification persisted when the Delivery
// Plan version was activated. Workers do not need GitHub credentials to read it.
func (s *Store) TicketBody(ctx context.Context, versionID string, issueID int64) (string, error) {
	if strings.TrimSpace(versionID) == "" || issueID == 0 {
		return "", ErrNotFound
	}
	var body string
	if err := s.db.QueryRowContext(ctx, `SELECT body FROM plan_tickets WHERE version_id = ? AND issue_id = ?`, versionID, issueID).Scan(&body); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}
	return body, nil
}

// ReadyFrontier is a read-only preview. ClaimReady repeats the same query and
// checks in its transaction, so a stale preview can never grant ownership.
func (s *Store) ReadyFrontier(ctx context.Context, versionID string, maxParallelRuns int, now time.Time) ([]plan.FrontierTicket, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	snapshot, err := loadFrontierTx(ctx, tx, versionID, now)
	if err != nil {
		return nil, err
	}
	snapshot.MaxParallel = maxParallelRuns
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return plan.ReadyFrontier(snapshot), nil
}

// ClaimReady is the execution boundary: eligibility, capacity, ownership,
// session reuse, run numbering, and lease generation are committed together.
func (s *Store) ClaimReady(ctx context.Context, request ClaimRequest) (TicketClaim, error) {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	return s.claimReady(ctx, request)
}

func (s *Store) claimReady(ctx context.Context, request ClaimRequest) (TicketClaim, error) {
	if request.VersionID == "" || request.Owner == "" || request.MaxParallelRuns <= 0 {
		return TicketClaim{}, ErrInvalidClaim
	}
	if request.LeaseTTL <= 0 {
		request.LeaseTTL = time.Minute
	}
	if request.Now.IsZero() {
		request.Now = time.Now().UTC()
	} else {
		request.Now = request.Now.UTC()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TicketClaim{}, err
	}
	defer tx.Rollback()
	snapshot, err := loadFrontierTx(ctx, tx, request.VersionID, request.Now)
	if err != nil {
		return TicketClaim{}, err
	}
	if snapshot.PlanState != plan.StateActive {
		return TicketClaim{}, fmt.Errorf("%w: plan version is %q", ErrNotReady, snapshot.PlanState)
	}

	byID := make(map[int64]plan.FrontierTicket, len(snapshot.Tickets))
	for _, ticket := range snapshot.Tickets {
		byID[ticket.IssueID] = ticket
	}
	if request.TicketID != 0 {
		ticket, exists := byID[request.TicketID]
		if !exists {
			return TicketClaim{}, ErrNotFound
		}
		if ticket.Owner != "" {
			return TicketClaim{}, ErrFencingConflict
		}
	}
	if snapshot.ActiveRuns >= request.MaxParallelRuns {
		return TicketClaim{}, ErrCapacity
	}

	// The requested ticket must be in the same capacity-sized frontier exposed
	// to callers; a request cannot turn a preview into an arbitrary dispatch.
	snapshot.MaxParallel = request.MaxParallelRuns
	ready := plan.ReadyFrontier(snapshot)
	var selected plan.FrontierTicket
	if request.TicketID == 0 {
		if len(ready) == 0 {
			return TicketClaim{}, ErrNoReadyTickets
		}
		selected = ready[0]
	} else {
		for _, ticket := range ready {
			if ticket.IssueID == request.TicketID {
				selected = ticket
				break
			}
		}
		if selected.IssueID == 0 {
			return TicketClaim{}, ErrNotReady
		}
	}

	var sessionID, currentOwner, sessionState, currentRunID, workspacePath, codexStatePath string
	var currentGeneration, recoveryEpoch int64
	existingSession := true
	err = tx.QueryRowContext(ctx, `SELECT session_id, owner, state, COALESCE(current_run_id, ''), current_lease_generation, workspace_path, codex_state_path
FROM ticket_sessions WHERE version_id = ? AND issue_id = ?`, request.VersionID, selected.IssueID).
		Scan(&sessionID, &currentOwner, &sessionState, &currentRunID, &currentGeneration, &workspacePath, &codexStatePath)
	if errors.Is(err, sql.ErrNoRows) {
		existingSession = false
		sessionID, err = randomID("ts-")
		if err != nil {
			return TicketClaim{}, err
		}
		nowText := formatTimestamp(request.Now)
		if _, err := tx.ExecContext(ctx, `INSERT INTO ticket_sessions(session_id, version_id, issue_id, owner, state, current_lease_generation, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, 0, ?, ?)`, sessionID, request.VersionID, selected.IssueID, request.Owner, SessionRunning, nowText, nowText); err != nil {
			return TicketClaim{}, err
		}
	} else if err != nil {
		return TicketClaim{}, err
	} else {
		if sessionState == SessionClosed {
			return TicketClaim{}, ErrNotReady
		}
		expiredAgentRunID := ""
		if currentRunID != "" {
			var runKind, runState, launchState, leaseToken, leaseState, expiresText string
			var runRecoveryEpoch int64
			if err := tx.QueryRowContext(ctx, `SELECT r.run_kind, r.state, r.launch_state, r.recovery_epoch, l.lease_token, l.state, l.expires_at
FROM worker_runs r JOIN run_leases l ON l.run_id = r.run_id AND l.generation = r.lease_generation
WHERE r.run_id = ?`, currentRunID).Scan(&runKind, &runState, &launchState, &runRecoveryEpoch, &leaseToken, &leaseState, &expiresText); err == nil {
				expiresAt, parseErr := time.Parse(time.RFC3339Nano, expiresText)
				live := parseErr == nil && runState == RunRunning && leaseState == LeaseActive && expiresAt.After(request.Now)
				if currentOwner != "" && live {
					return TicketClaim{}, ErrFencingConflict
				}
				if runKind == RunDelivery && runState == RunRunning {
					if launchState != "ready" {
						return TicketClaim{}, ErrNotReady
					}
					if err := recoverExpiredDeliveryTx(ctx, tx, request.VersionID, selected.IssueID, sessionID, currentRunID, runRecoveryEpoch, launchState, leaseToken, request.MaxAttempts, request.Now, nil); err != nil {
						return TicketClaim{}, err
					}
					if err := tx.Commit(); err != nil {
						return TicketClaim{}, err
					}
					return TicketClaim{}, ErrNotReady
				}
				if runKind == RunAgent && runState == RunRunning && !live {
					expiredAgentRunID = currentRunID
				}
			}
		}
		if request.ProvisionSession != nil {
			_, provisionErr := request.ProvisionSession(ctx, SessionProvisioning{SessionID: sessionID, Existing: true, WorkspacePath: workspacePath, CodexStatePath: codexStatePath, CurrentRunID: expiredAgentRunID})
			if provisionErr != nil {
				handled, failureErr := recordEstablishedSessionAuthenticationFailureTx(ctx, tx, request.VersionID, selected.IssueID, expiredAgentRunID, provisionErr, request.Now)
				if failureErr != nil {
					return TicketClaim{}, errors.Join(provisionErr, failureErr)
				}
				if handled {
					if err := tx.Commit(); err != nil {
						return TicketClaim{}, errors.Join(provisionErr, err)
					}
					return TicketClaim{}, &sessionAuthenticationTerminalizedError{cause: provisionErr}
				}
				return TicketClaim{}, provisionErr
			}
		}
		if currentRunID != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE worker_runs SET state = ? WHERE run_id = ? AND state = ?`, "superseded", currentRunID, RunRunning); err != nil {
				return TicketClaim{}, err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE run_leases SET state = ? WHERE run_id = ? AND state = ?`, "expired", currentRunID, LeaseActive); err != nil {
				return TicketClaim{}, err
			}
			result, err := tx.ExecContext(ctx, `UPDATE review_feedback_events SET claimed_run_id = '' WHERE claimed_run_id = ?`, currentRunID)
			if err != nil {
				return TicketClaim{}, err
			}
			claimedFeedback, err := result.RowsAffected()
			if err != nil {
				return TicketClaim{}, err
			}
			claimedRevalidations, err := releaseMergeReadyRevalidationsTx(ctx, tx, currentRunID)
			if err != nil {
				return TicketClaim{}, err
			}
			claimedFeedback += claimedRevalidations
			if claimedFeedback > 0 {
				if _, err := tx.ExecContext(ctx, `UPDATE ticket_runtime SET state = ?, updated_at = ? WHERE version_id = ? AND issue_id = ? AND delivered = 0`, plan.StateWaitingReview, formatTimestamp(request.Now), request.VersionID, selected.IssueID); err != nil {
					return TicketClaim{}, err
				}
				if err := tx.Commit(); err != nil {
					return TicketClaim{}, err
				}
				return TicketClaim{}, ErrNotReady
			}
		}
		currentGeneration++
		if _, err := tx.ExecContext(ctx, `UPDATE ticket_sessions SET owner = ?, state = ?, current_lease_generation = ?, updated_at = ? WHERE session_id = ?`, request.Owner, SessionRunning, currentGeneration, formatTimestamp(request.Now), sessionID); err != nil {
			return TicketClaim{}, err
		}
	}

	if ready, err := infrastructureRetryReadyTx(ctx, tx, sessionID, request.Now); err != nil {
		return TicketClaim{}, err
	} else if !ready {
		return TicketClaim{}, ErrNotReady
	}
	if currentGeneration == 0 {
		currentGeneration = 1
		if _, err := tx.ExecContext(ctx, `UPDATE ticket_sessions SET current_lease_generation = ?, updated_at = ? WHERE session_id = ?`, currentGeneration, formatTimestamp(request.Now), sessionID); err != nil {
			return TicketClaim{}, err
		}
	}
	if err := tx.QueryRowContext(ctx, `SELECT recovery_epoch FROM ticket_sessions WHERE session_id = ?`, sessionID).Scan(&recoveryEpoch); err != nil {
		return TicketClaim{}, err
	}
	var attempt int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(attempt), 0) + 1 FROM worker_runs WHERE session_id = ?`, sessionID).Scan(&attempt); err != nil {
		return TicketClaim{}, err
	}
	var attemptsInEpoch int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM worker_runs WHERE session_id = ? AND recovery_epoch = ? AND run_kind = ?`, sessionID, recoveryEpoch, RunAgent).Scan(&attemptsInEpoch); err != nil {
		return TicketClaim{}, err
	}
	if attemptsInEpoch >= maxWorkerAttempts(request.MaxAttempts) {
		reason, err := noProgressReasonTx(ctx, tx, sessionID, attemptsInEpoch)
		if err != nil {
			return TicketClaim{}, err
		}
		if err := markTicketNeedsAttentionTx(ctx, tx, request.VersionID, selected.IssueID, reason, request.Now); err != nil {
			return TicketClaim{}, err
		}
		if err := tx.Commit(); err != nil {
			return TicketClaim{}, err
		}
		return TicketClaim{}, ErrNotReady
	}
	var provisioned SessionProvisioningResult
	if !existingSession && request.ProvisionSession != nil {
		provisioning := SessionProvisioning{SessionID: sessionID, Existing: existingSession, WorkspacePath: workspacePath, CodexStatePath: codexStatePath}
		provisioned, err = request.ProvisionSession(ctx, provisioning)
		if err != nil {
			return TicketClaim{}, compensateSessionProvisioning(provisioned, err)
		}
	}
	runID, err := randomID("run-")
	if err != nil {
		return TicketClaim{}, compensateSessionProvisioning(provisioned, err)
	}
	leaseToken, err := randomID("lease-")
	if err != nil {
		return TicketClaim{}, compensateSessionProvisioning(provisioned, err)
	}
	expiresAt := request.Now.Add(request.LeaseTTL)
	if _, err := tx.ExecContext(ctx, `INSERT INTO worker_runs(run_id, session_id, attempt, recovery_epoch, lease_generation, state, started_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`, runID, sessionID, attempt, recoveryEpoch, currentGeneration, RunRunning, formatTimestamp(request.Now)); err != nil {
		return TicketClaim{}, compensateSessionProvisioning(provisioned, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO run_leases(lease_token, run_id, session_id, generation, state, expires_at, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`, leaseToken, runID, sessionID, currentGeneration, LeaseActive, formatTimestamp(expiresAt), formatTimestamp(request.Now)); err != nil {
		return TicketClaim{}, compensateSessionProvisioning(provisioned, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ticket_sessions SET current_run_id = ?, owner = ?, state = ?, current_lease_generation = ?, updated_at = ? WHERE session_id = ?`, runID, request.Owner, SessionRunning, currentGeneration, formatTimestamp(request.Now), sessionID); err != nil {
		return TicketClaim{}, compensateSessionProvisioning(provisioned, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO ticket_runtime(version_id, issue_id, state, delivered, updated_at)
VALUES (?, ?, ?, 0, ?)
ON CONFLICT(version_id, issue_id) DO UPDATE SET state = excluded.state, updated_at = excluded.updated_at`, request.VersionID, selected.IssueID, plan.StateRunning, formatTimestamp(request.Now)); err != nil {
		return TicketClaim{}, compensateSessionProvisioning(provisioned, err)
	}
	if err := recordDispatchFairnessTx(ctx, tx, request.fairnessRepository, request.fairnessRootIssueID, request.Now); err != nil {
		return TicketClaim{}, compensateSessionProvisioning(provisioned, err)
	}
	if err := tx.Commit(); err != nil {
		return TicketClaim{}, compensateSessionProvisioning(provisioned, err)
	}
	claim := TicketClaim{VersionID: request.VersionID, TicketID: selected.IssueID, TicketNumber: selected.Number, TicketTitle: selected.Title, Owner: request.Owner, SessionID: sessionID, RunID: runID, Attempt: attempt, LeaseToken: leaseToken, LeaseGeneration: currentGeneration, LeaseExpiresAt: expiresAt}
	if request.fairnessRepository != "" {
		claim.PlanRootNumber = request.fairnessRootNumber
	}
	return claim, nil
}

func compensateSessionProvisioning(provisioning SessionProvisioningResult, cause error) error {
	if provisioning.Rollback == nil {
		return cause
	}
	return errors.Join(cause, provisioning.Rollback())
}

func recordEstablishedSessionAuthenticationFailureTx(ctx context.Context, tx *sql.Tx, versionID string, issueID int64, runID string, provisionErr error, now time.Time) (bool, error) {
	var authenticationFailure *SessionAuthenticationFailure
	if !errors.As(provisionErr, &authenticationFailure) {
		return false, nil
	}
	if runID != "" {
		result, err := tx.ExecContext(ctx, `UPDATE worker_runs SET state = 'failed', finished_at = ? WHERE run_id = ? AND state = ?`, formatTimestamp(now), runID, RunRunning)
		if err != nil {
			return true, err
		}
		updated, err := result.RowsAffected()
		if err != nil || updated != 1 {
			return true, errors.Join(err, ErrInvalidClaim)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE run_leases SET state = 'expired' WHERE run_id = ? AND state = ?`, runID, LeaseActive); err != nil {
			return true, err
		}
		if authenticationFailure.DiagnosticsPath != "" {
			if _, err := tx.ExecContext(ctx, `INSERT INTO run_diagnostics(run_id, diagnostics_path, error, created_at) VALUES (?, ?, ?, ?) ON CONFLICT(run_id) DO NOTHING`, runID, authenticationFailure.DiagnosticsPath, ErrSessionAuthenticationUnavailable.Error(), formatTimestamp(now)); err != nil {
				return true, err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE review_feedback_events SET claimed_run_id = '' WHERE claimed_run_id = ?`, runID); err != nil {
			return true, err
		}
		if _, err := releaseMergeReadyRevalidationsTx(ctx, tx, runID); err != nil {
			return true, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE ticket_sessions SET consecutive_failures = consecutive_failures + 1, updated_at = ? WHERE session_id = (SELECT session_id FROM worker_runs WHERE run_id = ?)`, formatTimestamp(now), runID); err != nil {
			return true, err
		}
	}
	if err := markTicketNeedsAttentionTx(ctx, tx, versionID, issueID, ErrSessionAuthenticationUnavailable.Error(), now); err != nil {
		return true, err
	}
	return true, nil
}

func (s *Store) RecordRecoveryAuthenticationFailure(ctx context.Context, run RecoveryRun, diagnosticsPath string, now time.Time) error {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	if run.Claim.RunID == "" || run.Claim.LeaseToken == "" || run.Claim.LeaseGeneration <= 0 {
		return ErrInvalidClaim
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var currentRunID string
	err = tx.QueryRowContext(ctx, `SELECT s.current_run_id FROM worker_runs r
JOIN ticket_sessions s ON s.session_id = r.session_id
JOIN run_leases l ON l.run_id = r.run_id AND l.generation = r.lease_generation
WHERE r.run_id = ? AND l.lease_token = ? AND l.generation = ? AND r.state = ? AND l.state = ?`, run.Claim.RunID, run.Claim.LeaseToken, run.Claim.LeaseGeneration, RunRunning, LeaseActive).Scan(&currentRunID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrInvalidClaim
	}
	if err != nil {
		return err
	}
	if currentRunID != run.Claim.RunID {
		return ErrInvalidClaim
	}
	handled, err := recordEstablishedSessionAuthenticationFailureTx(ctx, tx, run.Claim.VersionID, run.Claim.TicketID, run.Claim.RunID, &SessionAuthenticationFailure{DiagnosticsPath: diagnosticsPath}, now)
	if err != nil {
		return err
	}
	if !handled {
		return ErrInvalidClaim
	}
	return tx.Commit()
}

func (s *Store) CurrentClaim(ctx context.Context, versionID string, issueID int64) (TicketClaim, error) {
	return s.currentClaimAt(ctx, versionID, issueID, time.Now().UTC())
}

func (s *Store) PendingDeliveryClaims(ctx context.Context, repository string, now time.Time) ([]TicketClaim, error) {
	if repository == "" {
		return nil, ErrInvalidClaim
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	rows, err := s.db.QueryContext(ctx, `SELECT s.version_id, s.issue_id, t.issue_number, t.title, s.owner, s.session_id, r.run_id, r.attempt, l.lease_token, l.generation, l.expires_at
FROM ticket_sessions s
JOIN worker_runs r ON r.run_id = s.current_run_id
JOIN run_leases l ON l.run_id = r.run_id AND l.generation = r.lease_generation
JOIN plan_versions v ON v.version_id = s.version_id
JOIN plans p ON p.id = v.plan_id
JOIN plan_tickets t ON t.version_id = s.version_id AND t.issue_id = s.issue_id
WHERE p.repository = ? AND s.state = ? AND r.run_kind = ? AND r.state = ? AND r.launch_state = 'ready' AND r.prelaunch_reserved = 0
AND l.state = ? AND l.expires_at > ? ORDER BY r.started_at, r.run_id`, repository, SessionRunning, RunDelivery, RunRunning, LeaseActive, formatTimestamp(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var claims []TicketClaim
	for rows.Next() {
		var claim TicketClaim
		var expiresText string
		if err := rows.Scan(&claim.VersionID, &claim.TicketID, &claim.TicketNumber, &claim.TicketTitle, &claim.Owner, &claim.SessionID, &claim.RunID, &claim.Attempt, &claim.LeaseToken, &claim.LeaseGeneration, &expiresText); err != nil {
			return nil, err
		}
		claim.LeaseExpiresAt, err = time.Parse(time.RFC3339Nano, expiresText)
		if err != nil {
			return nil, err
		}
		claims = append(claims, claim)
	}
	return claims, rows.Err()
}

func (s *Store) ClaimPendingDeliveryClaims(ctx context.Context, repository string, maxParallelRuns int, leaseTTL time.Duration, now time.Time) ([]TicketClaim, error) {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	if repository == "" || maxParallelRuns <= 0 {
		return nil, ErrInvalidClaim
	}
	if leaseTTL <= 0 {
		leaseTTL = time.Minute
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	activeRuns, err := activeRunCountTx(ctx, tx, now)
	if err != nil {
		return nil, err
	}
	available := maxParallelRuns - activeRuns
	if available <= 0 {
		return nil, tx.Commit()
	}
	rows, err := tx.QueryContext(ctx, `SELECT s.session_id
FROM ticket_sessions s
JOIN ticket_runtime rt ON rt.version_id = s.version_id AND rt.issue_id = s.issue_id
JOIN plan_versions v ON v.version_id = s.version_id
JOIN plans p ON p.id = v.plan_id
WHERE p.repository = ? AND p.current_version_id = s.version_id
AND s.state = ? AND s.accepted_commit != '' AND s.delivery_retry_pending = 1
AND rt.state = ? AND rt.delivered = 0
AND NOT EXISTS (SELECT 1 FROM infrastructure_retry_backoffs backoff WHERE backoff.session_id = s.session_id AND backoff.retry_at > ?)
ORDER BY s.updated_at, s.session_id LIMIT ?`, repository, SessionRunning, plan.StateWaitingReview, formatTimestamp(now), available)
	if err != nil {
		return nil, err
	}
	var sessionIDs []string
	for rows.Next() {
		var sessionID string
		if err := rows.Scan(&sessionID); err != nil {
			rows.Close()
			return nil, err
		}
		sessionIDs = append(sessionIDs, sessionID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	claims := make([]TicketClaim, 0, len(sessionIDs))
	for _, sessionID := range sessionIDs {
		result, err := tx.ExecContext(ctx, `UPDATE ticket_sessions SET delivery_retry_pending = 0, updated_at = ? WHERE session_id = ? AND delivery_retry_pending = 1`, formatTimestamp(now), sessionID)
		if err != nil {
			return nil, err
		}
		updated, err := result.RowsAffected()
		if err != nil {
			return nil, err
		}
		if updated != 1 {
			continue
		}
		claim, err := claimDeliveryControllerTx(ctx, tx, sessionID, leaseTTL, now, "")
		if err != nil {
			return nil, err
		}
		claims = append(claims, claim)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return claims, nil
}

func (s *Store) ReserveWorkerLaunch(ctx context.Context, claim TicketClaim, audit WorkerAudit, now time.Time) error {
	return s.reserveWorkerLaunch(ctx, claim, audit, now, "")
}

func (s *Store) ReserveDeliveryControllerPrelaunch(ctx context.Context, claim TicketClaim, now time.Time) error {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	if claim.RunID == "" || claim.LeaseToken == "" || claim.LeaseGeneration <= 0 {
		return ErrInvalidClaim
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	result, err := s.db.ExecContext(ctx, `UPDATE worker_runs
SET prelaunch_reserved = 1
WHERE run_id = ? AND lease_generation = ? AND run_kind = ? AND state = ? AND launch_state = 'ready' AND prelaunch_reserved = 0
AND EXISTS (
    SELECT 1 FROM ticket_sessions s
    JOIN run_leases l ON l.run_id = worker_runs.run_id AND l.generation = worker_runs.lease_generation
    WHERE s.current_run_id = worker_runs.run_id AND l.lease_token = ? AND l.state = ? AND l.expires_at > ?
)`, claim.RunID, claim.LeaseGeneration, RunDelivery, RunRunning, claim.LeaseToken, LeaseActive, formatTimestamp(now))
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrWorkerLaunched
	}
	return nil
}

func (s *Store) AcquireDeliveryControllerCreateFence(ctx context.Context, claim TicketClaim, now time.Time) (func(context.Context) error, error) {
	s.leaseMu.Lock()
	unlock := sync.OnceFunc(s.leaseMu.Unlock)
	if claim.VersionID == "" || claim.TicketID == 0 || claim.RunID == "" || claim.LeaseToken == "" || claim.LeaseGeneration <= 0 {
		unlock()
		return nil, ErrInvalidClaim
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		unlock()
		return nil, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE worker_runs
SET container_create_generation = container_create_generation + 1, container_create_pending = 1
WHERE run_id = ? AND lease_generation = ? AND run_kind = ? AND state = ?
AND launch_state = 'ready' AND prelaunch_reserved = 1 AND isolation_pending = 0 AND container_create_pending = 0
AND EXISTS (
    SELECT 1 FROM ticket_sessions s
    JOIN run_leases l ON l.run_id = worker_runs.run_id AND l.generation = worker_runs.lease_generation
    WHERE s.version_id = ? AND s.issue_id = ? AND s.current_run_id = worker_runs.run_id
      AND l.lease_token = ? AND l.state = ? AND l.expires_at > ?
	)`, claim.RunID, claim.LeaseGeneration, RunDelivery, RunRunning, claim.VersionID, claim.TicketID, claim.LeaseToken, LeaseActive, formatTimestamp(now))
	if err != nil {
		unlock()
		return nil, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		unlock()
		return nil, err
	}
	if count != 1 {
		var expiredReady int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
SELECT 1 FROM worker_runs r
JOIN ticket_sessions s ON s.current_run_id = r.run_id
JOIN run_leases l ON l.run_id = r.run_id AND l.generation = r.lease_generation
WHERE s.version_id = ? AND s.issue_id = ? AND r.run_id = ? AND r.lease_generation = ?
AND r.run_kind = ? AND r.state = ? AND r.launch_state = 'ready' AND r.prelaunch_reserved = 1
	AND r.isolation_pending = 0 AND l.lease_token = ? AND l.state = ? AND l.expires_at <= ?
)`, claim.VersionID, claim.TicketID, claim.RunID, claim.LeaseGeneration, RunDelivery, RunRunning, claim.LeaseToken, LeaseActive, formatTimestamp(now)).Scan(&expiredReady); err != nil {
			unlock()
			return nil, err
		}
		unlock()
		if expiredReady != 0 {
			return nil, ErrDeliveryLaunchLeaseExpired
		}
		return nil, ErrWorkerLaunched
	}
	if err := tx.Commit(); err != nil {
		unlock()
		return nil, err
	}
	var once sync.Once
	var releaseErr error
	release := func(releaseCtx context.Context) error {
		once.Do(func() {
			defer unlock()
			result, err := s.db.ExecContext(releaseCtx, `UPDATE worker_runs SET container_create_pending = 0
WHERE run_id = ? AND lease_generation = ? AND container_create_pending = 1 AND isolation_pending = 0`, claim.RunID, claim.LeaseGeneration)
			if err != nil {
				releaseErr = err
				return
			}
			count, err := result.RowsAffected()
			if err != nil {
				releaseErr = err
				return
			}
			if count != 1 {
				releaseErr = ErrFencingConflict
			}
		})
		return releaseErr
	}
	return release, nil
}

func (s *Store) ReserveDeliveryControllerLaunch(ctx context.Context, claim TicketClaim, audit WorkerAudit, now time.Time) error {
	return s.reserveWorkerLaunch(ctx, claim, audit, now, RunDelivery)
}

func (s *Store) reserveWorkerLaunch(ctx context.Context, claim TicketClaim, audit WorkerAudit, now time.Time, runKind RunKind) error {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	if claim.RunID == "" || claim.LeaseToken == "" || claim.LeaseGeneration <= 0 || audit.RunID != claim.RunID || audit.LeaseGeneration != claim.LeaseGeneration || audit.ContainerID != "" || audit.ImageDigest == "" || !validWorkerToolVersions(audit.ToolVersions) || audit.GitHubWriteCredentials {
		return ErrInvalidClaim
	}
	mounts, err := json.Marshal(audit.Mounts)
	if err != nil {
		return err
	}
	versions, err := json.Marshal(audit.ToolVersions)
	if err != nil {
		return err
	}
	extraHosts, err := json.Marshal(audit.ExtraHosts)
	if err != nil {
		return err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE worker_runs
SET launch_state = 'launched', launched_at = ?
WHERE run_id = ? AND lease_generation = ? AND state = ? AND launch_state = 'ready'
AND (? = '' OR (run_kind = ? AND prelaunch_reserved = 1))
AND (? = '' OR isolation_pending = 0)
AND EXISTS (
    SELECT 1 FROM ticket_sessions s
    JOIN run_leases l ON l.run_id = worker_runs.run_id AND l.generation = worker_runs.lease_generation
    WHERE s.current_run_id = worker_runs.run_id AND l.lease_token = ? AND l.state = ? AND l.expires_at > ?
)`, formatTimestamp(now), claim.RunID, claim.LeaseGeneration, RunRunning, runKind, runKind, runKind, claim.LeaseToken, LeaseActive, formatTimestamp(now))
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		if runKind == RunDelivery {
			var expiredReady int
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
SELECT 1 FROM worker_runs r
JOIN ticket_sessions s ON s.current_run_id = r.run_id
JOIN run_leases l ON l.run_id = r.run_id AND l.generation = r.lease_generation
WHERE r.run_id = ? AND r.lease_generation = ? AND r.run_kind = ? AND r.state = ?
AND r.launch_state = 'ready' AND r.prelaunch_reserved = 1
AND l.lease_token = ? AND l.state = ? AND l.expires_at <= ?
)`, claim.RunID, claim.LeaseGeneration, RunDelivery, RunRunning, claim.LeaseToken, LeaseActive, formatTimestamp(now)).Scan(&expiredReady); err != nil {
				return err
			}
			if expiredReady != 0 {
				return ErrDeliveryLaunchLeaseExpired
			}
		}
		return ErrWorkerLaunched
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO worker_audits(run_id, lease_generation, container_id, image_digest, mounts_json, extra_hosts_json, tool_versions_json, github_write_credentials, created_at)
VALUES (?, ?, '', ?, ?, ?, ?, 0, ?)`, audit.RunID, audit.LeaseGeneration, audit.ImageDigest, string(mounts), string(extraHosts), string(versions), formatTimestamp(now)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RequireCurrentDeliveryLease(ctx context.Context, claim TicketClaim, now time.Time) error {
	if claim.VersionID == "" || claim.TicketID == 0 || claim.RunID == "" || claim.LeaseToken == "" || claim.LeaseGeneration <= 0 {
		return ErrInvalidClaim
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	var current bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(
SELECT 1
FROM ticket_sessions s
JOIN worker_runs r ON r.run_id = s.current_run_id
JOIN run_leases l ON l.run_id = r.run_id AND l.generation = r.lease_generation
WHERE s.version_id = ? AND s.issue_id = ? AND r.run_id = ? AND r.run_kind = ?
AND r.state = ? AND l.lease_token = ? AND l.generation = ? AND l.state = ? AND l.expires_at > ?
)`, claim.VersionID, claim.TicketID, claim.RunID, RunDelivery, RunRunning, claim.LeaseToken, claim.LeaseGeneration, LeaseActive, formatTimestamp(now)).Scan(&current)
	if err != nil {
		return err
	}
	if !current {
		return ErrInvalidClaim
	}
	return nil
}

func (s *Store) WithCurrentAgentLease(ctx context.Context, claim TicketClaim, now time.Time, operation func() error) (bool, error) {
	if claim.VersionID == "" || claim.TicketID == 0 || claim.RunID == "" || claim.LeaseToken == "" || claim.LeaseGeneration <= 0 || operation == nil {
		return false, ErrInvalidClaim
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	var current bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(
SELECT 1
FROM ticket_sessions s
JOIN worker_runs r ON r.run_id = s.current_run_id
JOIN run_leases l ON l.run_id = r.run_id AND l.generation = r.lease_generation
WHERE s.version_id = ? AND s.issue_id = ? AND r.run_id = ? AND r.run_kind = ?
AND r.state = ? AND l.lease_token = ? AND l.generation = ? AND l.state = ? AND l.expires_at > ?
)`, claim.VersionID, claim.TicketID, claim.RunID, RunAgent, RunRunning, claim.LeaseToken, claim.LeaseGeneration, LeaseActive, formatTimestamp(now)).Scan(&current)
	if err != nil {
		return false, err
	}
	if !current {
		return false, nil
	}
	return true, operation()
}

func (s *Store) RecoveryOwner(ctx context.Context, versionID string, issueID int64) (string, error) {
	var owner string
	err := s.db.QueryRowContext(ctx, `SELECT s.owner
FROM ticket_sessions s
JOIN ticket_runtime rt ON rt.version_id = s.version_id AND rt.issue_id = s.issue_id
WHERE s.version_id = ? AND s.issue_id = ? AND s.state = ? AND rt.state = ? AND s.owner != ''`, versionID, issueID, SessionRunning, plan.StateRunning).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return owner, err
}

func (s *Store) currentClaimAt(ctx context.Context, versionID string, issueID int64, now time.Time) (TicketClaim, error) {
	var claim TicketClaim
	var expiresText string
	err := s.db.QueryRowContext(ctx, `SELECT s.session_id, s.owner, s.current_run_id, r.attempt, l.lease_token, l.generation, l.expires_at, t.issue_number, t.title
FROM ticket_sessions s
JOIN worker_runs r ON r.run_id = s.current_run_id
JOIN run_leases l ON l.run_id = r.run_id AND l.generation = r.lease_generation
JOIN plan_tickets t ON t.version_id = s.version_id AND t.issue_id = s.issue_id
WHERE s.version_id = ? AND s.issue_id = ? AND s.state = ? AND r.run_kind = ? AND r.state = ? AND l.state = ? AND l.expires_at > ?`, versionID, issueID, SessionRunning, RunAgent, RunRunning, LeaseActive, formatTimestamp(now)).
		Scan(&claim.SessionID, &claim.Owner, &claim.RunID, &claim.Attempt, &claim.LeaseToken, &claim.LeaseGeneration, &expiresText, &claim.TicketNumber, &claim.TicketTitle)
	if errors.Is(err, sql.ErrNoRows) {
		return TicketClaim{}, ErrNotFound
	}
	if err != nil {
		return TicketClaim{}, err
	}
	claim.VersionID = versionID
	claim.TicketID = issueID
	claim.LeaseExpiresAt, err = time.Parse(time.RFC3339Nano, expiresText)
	return claim, err
}

func (s *Store) ClaimReviewRevision(ctx context.Context, versionID string, issueID int64, leaseTTL time.Duration, now time.Time, maxParallelRuns int, maxAttempts ...int) (TicketClaim, error) {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	if versionID == "" || issueID == 0 || maxParallelRuns <= 0 {
		return TicketClaim{}, ErrInvalidClaim
	}
	if leaseTTL <= 0 {
		leaseTTL = time.Minute
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TicketClaim{}, err
	}
	defer tx.Rollback()
	if frozen, err := planFrozenTx(ctx, tx, versionID); err != nil {
		return TicketClaim{}, err
	} else if frozen {
		return TicketClaim{}, ErrNotReady
	}
	var sessionID, owner, sessionState, runtimeState string
	var deliveryRetryPending int
	var ticketNumber int64
	var ticketTitle string
	var generation, recoveryEpoch int64
	err = tx.QueryRowContext(ctx, `SELECT s.session_id, s.owner, s.state, s.current_lease_generation, s.delivery_retry_pending, rt.state, t.issue_number, t.title
FROM ticket_sessions s
JOIN ticket_runtime rt ON rt.version_id = s.version_id AND rt.issue_id = s.issue_id
JOIN plan_tickets t ON t.version_id = s.version_id AND t.issue_id = s.issue_id
WHERE s.version_id = ? AND s.issue_id = ?`, versionID, issueID).Scan(&sessionID, &owner, &sessionState, &generation, &deliveryRetryPending, &runtimeState, &ticketNumber, &ticketTitle)
	if errors.Is(err, sql.ErrNoRows) {
		return TicketClaim{}, ErrNotFound
	}
	if err != nil {
		return TicketClaim{}, err
	}
	if sessionState != SessionRunning || runtimeState != plan.StateWaitingReview || owner == "" || deliveryRetryPending != 0 {
		return TicketClaim{}, ErrNotReady
	}
	if ready, err := infrastructureRetryReadyTx(ctx, tx, sessionID, now); err != nil {
		return TicketClaim{}, err
	} else if !ready {
		return TicketClaim{}, ErrNotReady
	}
	activeRuns, err := activeRunCountTx(ctx, tx, now)
	if err != nil {
		return TicketClaim{}, err
	}
	if activeRuns >= maxParallelRuns {
		return TicketClaim{}, ErrCapacity
	}
	if err := tx.QueryRowContext(ctx, `SELECT recovery_epoch FROM ticket_sessions WHERE session_id = ?`, sessionID).Scan(&recoveryEpoch); err != nil {
		return TicketClaim{}, err
	}
	generation++
	attempt := 0
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(attempt), 0) + 1 FROM worker_runs WHERE session_id = ?`, sessionID).Scan(&attempt); err != nil {
		return TicketClaim{}, err
	}
	limit := DefaultMaxWorkerAttempts
	if len(maxAttempts) > 0 {
		limit = maxWorkerAttempts(maxAttempts[0])
	}
	var attemptsInEpoch int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM worker_runs WHERE session_id = ? AND recovery_epoch = ? AND run_kind = ?`, sessionID, recoveryEpoch, RunAgent).Scan(&attemptsInEpoch); err != nil {
		return TicketClaim{}, err
	}
	if attemptsInEpoch >= limit {
		reason, err := noProgressReasonTx(ctx, tx, sessionID, attemptsInEpoch)
		if err != nil {
			return TicketClaim{}, err
		}
		if err := markTicketNeedsAttentionTx(ctx, tx, versionID, issueID, reason, now); err != nil {
			return TicketClaim{}, err
		}
		if err := tx.Commit(); err != nil {
			return TicketClaim{}, err
		}
		return TicketClaim{}, ErrNotReady
	}
	runID, err := randomID("run-")
	if err != nil {
		return TicketClaim{}, err
	}
	leaseToken, err := randomID("lease-")
	if err != nil {
		return TicketClaim{}, err
	}
	expiresAt := now.Add(leaseTTL)
	if _, err := tx.ExecContext(ctx, `INSERT INTO worker_runs(run_id, session_id, attempt, recovery_epoch, lease_generation, state, started_at) VALUES (?, ?, ?, ?, ?, ?, ?)`, runID, sessionID, attempt, recoveryEpoch, generation, RunRunning, formatTimestamp(now)); err != nil {
		return TicketClaim{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO run_leases(lease_token, run_id, session_id, generation, state, expires_at, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`, leaseToken, runID, sessionID, generation, LeaseActive, formatTimestamp(expiresAt), formatTimestamp(now)); err != nil {
		return TicketClaim{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ticket_sessions SET current_run_id = ?, current_lease_generation = ?, updated_at = ? WHERE session_id = ?`, runID, generation, formatTimestamp(now), sessionID); err != nil {
		return TicketClaim{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ticket_runtime SET state = ?, updated_at = ? WHERE version_id = ? AND issue_id = ? AND delivered = 0`, plan.StateRunning, formatTimestamp(now), versionID, issueID); err != nil {
		return TicketClaim{}, err
	}
	if err := tx.Commit(); err != nil {
		return TicketClaim{}, err
	}
	return TicketClaim{VersionID: versionID, TicketID: issueID, TicketNumber: ticketNumber, TicketTitle: ticketTitle, Owner: owner, SessionID: sessionID, RunID: runID, Attempt: attempt, LeaseToken: leaseToken, LeaseGeneration: generation, LeaseExpiresAt: expiresAt}, nil
}

func planFrozenTx(ctx context.Context, tx frontierRows, versionID string) (bool, error) {
	var frozen int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM plan_freezes WHERE version_id = ?`, versionID).Scan(&frozen)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func infrastructureRetryReadyTx(ctx context.Context, tx *sql.Tx, sessionID string, now time.Time) (bool, error) {
	var retryText string
	err := tx.QueryRowContext(ctx, `SELECT retry_at FROM infrastructure_retry_backoffs WHERE session_id = ?`, sessionID).Scan(&retryText)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	retryAt, err := time.Parse(time.RFC3339Nano, retryText)
	if err != nil {
		return false, err
	}
	return !retryAt.After(now), nil
}

func noProgressReasonTx(ctx context.Context, tx *sql.Tx, sessionID string, attempts int) (string, error) {
	reason := fmt.Sprintf("worker retry budget exhausted after %d attempts without a new accepted Candidate Revision", attempts)
	var latest string
	err := tx.QueryRowContext(ctx, `SELECT failure.reason
FROM run_failures failure
JOIN worker_runs run ON run.run_id = failure.run_id
WHERE run.session_id = ?
ORDER BY failure.recorded_at DESC, failure.run_id DESC
LIMIT 1`, sessionID).Scan(&latest)
	if errors.Is(err, sql.ErrNoRows) {
		return reason, nil
	}
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(latest) != "" {
		reason += ": " + latest
	}
	return reason, nil
}

// MarkTicketDelivered records the durable delivery fact that unlocks dependent
// tickets. The caller is expected to have already verified the merged revision
// and reachability at the GitHub boundary.
func (s *Store) MarkTicketDelivered(ctx context.Context, versionID string, issueID int64) error {
	_, err := s.markTicketDelivered(ctx, versionID, issueID, "", nil)
	return err
}

// MarkTicketDeliveredAtMerge records the merge revision with the Delivered
// fact. A repeated reconciliation leaves the original delivery fact unchanged.
func (s *Store) MarkTicketDeliveredAtMerge(ctx context.Context, versionID string, issueID int64, mergeCommit string) (bool, error) {
	if mergeCommit == "" {
		return false, ErrInvalidClaim
	}
	return s.markTicketDelivered(ctx, versionID, issueID, mergeCommit, nil)
}

func (s *Store) DeliveryContainerIsolationTarget(ctx context.Context, versionID string, issueID int64) (TicketClaim, error) {
	if versionID == "" || issueID == 0 {
		return TicketClaim{}, ErrInvalidClaim
	}
	var claim TicketClaim
	var expiresText string
	err := s.db.QueryRowContext(ctx, `SELECT s.version_id, s.issue_id, s.session_id, r.run_id, r.lease_generation, r.container_create_generation, l.lease_token, l.expires_at
FROM ticket_sessions s
JOIN worker_runs r ON r.run_id = s.current_run_id
JOIN run_leases l ON l.run_id = r.run_id AND l.generation = r.lease_generation
WHERE s.version_id = ? AND s.issue_id = ? AND r.run_kind = ? AND r.state = ?
AND (r.launch_state = 'launched' OR (r.launch_state = 'ready' AND r.prelaunch_reserved = 1))`, versionID, issueID, RunDelivery, RunRunning).
		Scan(&claim.VersionID, &claim.TicketID, &claim.SessionID, &claim.RunID, &claim.LeaseGeneration, &claim.IsolationGeneration, &claim.LeaseToken, &expiresText)
	if errors.Is(err, sql.ErrNoRows) {
		return TicketClaim{}, ErrNotFound
	}
	if err != nil {
		return TicketClaim{}, err
	}
	claim.LeaseExpiresAt, err = time.Parse(time.RFC3339Nano, expiresText)
	if err != nil {
		return TicketClaim{}, err
	}
	return claim, nil
}

func (s *Store) MarkTicketDeliveredAtMergeAfterIsolation(ctx context.Context, versionID string, issueID int64, mergeCommit string, isolated DeliveryIsolationProof) (bool, error) {
	if mergeCommit == "" {
		return false, ErrInvalidClaim
	}
	return s.markTicketDelivered(ctx, versionID, issueID, mergeCommit, []DeliveryIsolationProof{isolated})
}

func (s *Store) markTicketDelivered(ctx context.Context, versionID string, issueID int64, mergeCommit string, isolated []DeliveryIsolationProof) (bool, error) {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	nowText := formatTimestamp(now)
	if err := requireDeliveryIsolationTx(ctx, tx, versionID, map[int64]bool{issueID: true}, isolated); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE ticket_runtime SET state = ?, delivered = 1, updated_at = ?
WHERE version_id = ? AND issue_id = ? AND delivered = 0`, plan.StateDelivered, nowText, versionID, issueID)
	if err != nil {
		return false, err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		var repository string
		if err := tx.QueryRowContext(ctx, `SELECT p.repository FROM plan_tickets ticket
JOIN plan_versions version ON version.version_id = ticket.version_id
JOIN plans p ON p.id = version.plan_id
WHERE ticket.version_id = ? AND ticket.issue_id = ?`, versionID, issueID).Scan(&repository); errors.Is(err, sql.ErrNoRows) {
			return false, ErrNotFound
		} else if err != nil {
			return false, err
		}
		resolved, err := resolveDeliveredAttentionTx(ctx, tx, versionID, issueID, nowText)
		if err != nil {
			return false, err
		}
		if resolved > 0 {
			if _, err := s.queueWorkflowInboxProjectionTransitionTx(ctx, tx, repository, now); err != nil {
				return false, err
			}
		}
		if err := tx.Commit(); err != nil {
			return false, err
		}
		return false, nil
	}
	if mergeCommit != "" {
		result, err := tx.ExecContext(ctx, `UPDATE ticket_deliveries SET merge_commit = ?, updated_at = ? WHERE version_id = ? AND issue_id = ?`, mergeCommit, nowText, versionID, issueID)
		if err != nil {
			return false, err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return false, ErrNotFound
		}
	}
	resolvedAttention, err := resolveDeliveredAttentionTx(ctx, tx, versionID, issueID, nowText)
	if err != nil {
		return false, err
	}
	var sessionID, runID string
	sessionErr := tx.QueryRowContext(ctx, `SELECT session_id, COALESCE(current_run_id, '') FROM ticket_sessions WHERE version_id = ? AND issue_id = ?`, versionID, issueID).Scan(&sessionID, &runID)
	if sessionErr != nil && !errors.Is(sessionErr, sql.ErrNoRows) {
		return false, sessionErr
	}
	if sessionErr == nil && runID != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE worker_runs SET state = ?, finished_at = ? WHERE run_id = ? AND state = ?`, "succeeded", nowText, runID, RunRunning); err != nil {
			return false, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE run_leases SET state = ? WHERE run_id = ? AND state = ?`, "revoked", runID, LeaseActive); err != nil {
			return false, err
		}
	}
	if sessionErr == nil {
		if _, err := tx.ExecContext(ctx, `UPDATE ticket_sessions SET state = ?, owner = '', updated_at = ? WHERE session_id = ?`, SessionClosed, nowText, sessionID); err != nil {
			return false, err
		}
	}
	completionQueued, err := s.markPlanCompletedTx(ctx, tx, versionID, now)
	if err != nil {
		return false, err
	}
	if resolvedAttention > 0 && !completionQueued {
		if err := s.queueDeliveredQuestionRepairTx(ctx, tx, versionID, now); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func resolveDeliveredAttentionTx(ctx context.Context, tx *sql.Tx, versionID string, issueID int64, nowText string) (int64, error) {
	result, err := tx.ExecContext(ctx, `UPDATE workflow_questions
SET state = 'answered', answer = ?, answered_at = ?
WHERE version_id = ? AND issue_id = ? AND kind IN ('needs_attention', 'quality_gate', 'closed_unmerged_impact') AND state = 'open'`, "resolved by delivery", nowText, versionID, issueID)
	if err != nil {
		return 0, err
	}
	resolved, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	result, err = tx.ExecContext(ctx, `DELETE FROM plan_freezes WHERE version_id = ? AND issue_id = ?`, versionID, issueID)
	if err != nil {
		return 0, err
	}
	cleared, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return resolved + cleared, nil
}

func (s *Store) queueDeliveredQuestionRepairTx(ctx context.Context, tx *sql.Tx, versionID string, now time.Time) error {
	var repository string
	if err := tx.QueryRowContext(ctx, `SELECT p.repository FROM plans p JOIN plan_versions version ON version.plan_id = p.id WHERE version.version_id = ?`, versionID).Scan(&repository); err != nil {
		return err
	}
	_, err := s.queueWorkflowInboxProjectionTransitionTx(ctx, tx, repository, now)
	return err
}

func (s *Store) markPlanCompletedTx(ctx context.Context, tx *sql.Tx, versionID string, now time.Time) (bool, error) {
	var remaining, liveRuns int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM ticket_runtime WHERE version_id = ? AND delivered = 0`, versionID).Scan(&remaining); err != nil {
		return false, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM worker_runs r JOIN ticket_sessions s ON s.session_id = r.session_id WHERE s.version_id = ? AND r.state = ?`, versionID, RunRunning).Scan(&liveRuns); err != nil {
		return false, err
	}
	if remaining == 0 && liveRuns == 0 {
		result, err := tx.ExecContext(ctx, `INSERT INTO completed_plan_versions(version_id, completed_at) VALUES (?, ?) ON CONFLICT(version_id) DO NOTHING`, versionID, formatTimestamp(now))
		if err != nil {
			return false, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO plan_terminal_states(version_id, state, recorded_at) VALUES (?, ?, ?) ON CONFLICT(version_id) DO NOTHING`, versionID, StateCompleted, formatTimestamp(now)); err != nil {
			return false, err
		}
		if count, _ := result.RowsAffected(); count > 0 {
			var repository string
			if err := tx.QueryRowContext(ctx, `SELECT p.repository FROM plans p JOIN plan_versions v ON v.plan_id = p.id WHERE v.version_id = ?`, versionID).Scan(&repository); err != nil {
				return false, err
			}
			if _, err := s.queueWorkflowInboxProjectionTransitionTx(ctx, tx, repository, now); err != nil {
				return false, err
			}
			return true, nil
		}
	}
	return false, nil
}

type frontierRows interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func loadFrontierTx(ctx context.Context, tx frontierRows, versionID string, now time.Time) (plan.FrontierSnapshot, error) {
	var snapshot plan.FrontierSnapshot
	var versionState, planState string
	err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT t.state FROM plan_terminal_states t WHERE t.version_id = v.version_id), CASE WHEN EXISTS (SELECT 1 FROM completed_plan_versions c WHERE c.version_id = v.version_id) THEN ? ELSE v.state END), p.state FROM plan_versions v JOIN plans p ON p.id = v.plan_id WHERE v.version_id = ?`, StateCompleted, versionID).Scan(&versionState, &planState)
	if errors.Is(err, sql.ErrNoRows) {
		return snapshot, ErrNotFound
	}
	if err != nil {
		return snapshot, err
	}
	snapshot.VersionID = versionID
	frozen, err := planFrozenTx(ctx, tx, versionID)
	if err != nil {
		return snapshot, err
	}
	if frozen {
		snapshot.PlanState = plan.StateNeedsAttention
	} else if versionState == StateActive && planState == StateActive {
		snapshot.PlanState = plan.StateActive
	} else {
		snapshot.PlanState = versionState
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	rows, err := tx.QueryContext(ctx, `SELECT t.issue_id, t.issue_number, t.title, COALESCE(rt.delivered, t.delivered), COALESCE(rt.state, ''),
COALESCE(s.owner, ''), COALESCE(s.state, ''), COALESCE(r.state, ''), COALESCE(l.state, ''), COALESCE(l.expires_at, '')
FROM plan_tickets t
LEFT JOIN ticket_runtime rt ON rt.version_id = t.version_id AND rt.issue_id = t.issue_id
LEFT JOIN ticket_sessions s ON s.version_id = t.version_id AND s.issue_id = t.issue_id
LEFT JOIN worker_runs r ON r.run_id = s.current_run_id
LEFT JOIN run_leases l ON l.run_id = r.run_id AND l.generation = r.lease_generation
WHERE t.version_id = ?
  AND NOT EXISTS (SELECT 1 FROM plan_amendment_pauses pause JOIN plan_amendments amendment ON amendment.amendment_id = pause.amendment_id WHERE pause.version_id = t.version_id AND pause.issue_id = t.issue_id AND amendment.state = 'pending')`, versionID)
	if err != nil {
		return snapshot, err
	}
	defer rows.Close()
	for rows.Next() {
		var ticket plan.FrontierTicket
		var delivered int
		var runtimeState, owner, sessionState, runState, leaseState, expiresText string
		if err := rows.Scan(&ticket.IssueID, &ticket.Number, &ticket.Title, &delivered, &runtimeState, &owner, &sessionState, &runState, &leaseState, &expiresText); err != nil {
			return snapshot, err
		}
		ticket.Delivered = delivered != 0
		if runtimeState != "" && runtimeState != plan.StateQueued && runtimeState != plan.StateRunning && delivered == 0 {
			ticket.Owner = runtimeState
		}
		if sessionState == SessionRunning && runState == RunRunning && leaseState == LeaseActive {
			expiresAt, parseErr := time.Parse(time.RFC3339Nano, expiresText)
			if parseErr == nil && expiresAt.After(now) {
				ticket.Owner = owner
			}
		}
		snapshot.Tickets = append(snapshot.Tickets, ticket)
	}
	if err := rows.Err(); err != nil {
		return snapshot, err
	}
	snapshot.Dependencies = make(map[int64][]int64)
	dependencyRows, err := tx.QueryContext(ctx, `SELECT blocked_issue_id, blocker_issue_id FROM plan_dependencies WHERE version_id = ?`, versionID)
	if err != nil {
		return snapshot, err
	}
	defer dependencyRows.Close()
	for dependencyRows.Next() {
		var blocked, blocker int64
		if err := dependencyRows.Scan(&blocked, &blocker); err != nil {
			return snapshot, err
		}
		snapshot.Dependencies[blocked] = append(snapshot.Dependencies[blocked], blocker)
	}
	if err := dependencyRows.Err(); err != nil {
		return snapshot, err
	}
	if snapshot.ActiveRuns, err = activeRunCountTx(ctx, tx, now); err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

func activeRunCountTx(ctx context.Context, tx frontierRows, now time.Time) (int, error) {
	var activeRuns int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM worker_runs r
JOIN run_leases l ON l.run_id = r.run_id AND l.generation = r.lease_generation
WHERE r.state = ? AND l.state = ? AND l.expires_at > ?`, RunRunning, LeaseActive, formatTimestamp(now)).Scan(&activeRuns)
	return activeRuns, err
}

func randomID(prefix string) (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(buffer), nil
}
