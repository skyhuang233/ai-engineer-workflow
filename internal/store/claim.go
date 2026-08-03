package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/skyhuang233/workflow/internal/plan"
)

type ClaimRequest struct {
	VersionID       string
	TicketID        int64
	Owner           string
	MaxParallelRuns int
	LeaseTTL        time.Duration
	Now             time.Time
}

type TicketClaim struct {
	VersionID       string
	TicketID        int64
	TicketNumber    int64
	TicketTitle     string
	Owner           string
	SessionID       string
	RunID           string
	Attempt         int
	LeaseToken      string
	LeaseGeneration int64
	LeaseExpiresAt  time.Time
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

	var sessionID, currentOwner, sessionState, currentRunID string
	var currentGeneration int64
	err = tx.QueryRowContext(ctx, `SELECT session_id, owner, state, COALESCE(current_run_id, ''), current_lease_generation
FROM ticket_sessions WHERE version_id = ? AND issue_id = ?`, request.VersionID, selected.IssueID).
		Scan(&sessionID, &currentOwner, &sessionState, &currentRunID, &currentGeneration)
	if errors.Is(err, sql.ErrNoRows) {
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
		if currentOwner != "" && currentRunID != "" {
			var leaseState, expiresText string
			if err := tx.QueryRowContext(ctx, `SELECT state, expires_at FROM run_leases WHERE run_id = ? ORDER BY generation DESC LIMIT 1`, currentRunID).Scan(&leaseState, &expiresText); err == nil {
				expiresAt, parseErr := time.Parse(time.RFC3339Nano, expiresText)
				if parseErr == nil && leaseState == LeaseActive && expiresAt.After(request.Now) {
					return TicketClaim{}, ErrFencingConflict
				}
			}
		}
		if currentRunID != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE worker_runs SET state = ? WHERE run_id = ? AND state = ?`, "superseded", currentRunID, RunRunning); err != nil {
				return TicketClaim{}, err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE run_leases SET state = ? WHERE run_id = ? AND state = ?`, "expired", currentRunID, LeaseActive); err != nil {
				return TicketClaim{}, err
			}
		}
		currentGeneration++
		if _, err := tx.ExecContext(ctx, `UPDATE ticket_sessions SET owner = ?, state = ?, current_lease_generation = ?, updated_at = ? WHERE session_id = ?`, request.Owner, SessionRunning, currentGeneration, formatTimestamp(request.Now), sessionID); err != nil {
			return TicketClaim{}, err
		}
	}

	if currentGeneration == 0 {
		currentGeneration = 1
		if _, err := tx.ExecContext(ctx, `UPDATE ticket_sessions SET current_lease_generation = ?, updated_at = ? WHERE session_id = ?`, currentGeneration, formatTimestamp(request.Now), sessionID); err != nil {
			return TicketClaim{}, err
		}
	}
	var attempt int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(attempt), 0) + 1 FROM worker_runs WHERE session_id = ?`, sessionID).Scan(&attempt); err != nil {
		return TicketClaim{}, err
	}
	runID, err := randomID("run-")
	if err != nil {
		return TicketClaim{}, err
	}
	leaseToken, err := randomID("lease-")
	if err != nil {
		return TicketClaim{}, err
	}
	expiresAt := request.Now.Add(request.LeaseTTL)
	if _, err := tx.ExecContext(ctx, `INSERT INTO worker_runs(run_id, session_id, attempt, lease_generation, state, started_at)
VALUES (?, ?, ?, ?, ?, ?)`, runID, sessionID, attempt, currentGeneration, RunRunning, formatTimestamp(request.Now)); err != nil {
		return TicketClaim{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO run_leases(lease_token, run_id, session_id, generation, state, expires_at, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`, leaseToken, runID, sessionID, currentGeneration, LeaseActive, formatTimestamp(expiresAt), formatTimestamp(request.Now)); err != nil {
		return TicketClaim{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ticket_sessions SET current_run_id = ?, owner = ?, state = ?, current_lease_generation = ?, updated_at = ? WHERE session_id = ?`, runID, request.Owner, SessionRunning, currentGeneration, formatTimestamp(request.Now), sessionID); err != nil {
		return TicketClaim{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO ticket_runtime(version_id, issue_id, state, delivered, updated_at)
VALUES (?, ?, ?, 0, ?)
ON CONFLICT(version_id, issue_id) DO UPDATE SET state = excluded.state, updated_at = excluded.updated_at`, request.VersionID, selected.IssueID, plan.StateRunning, formatTimestamp(request.Now)); err != nil {
		return TicketClaim{}, err
	}
	if err := tx.Commit(); err != nil {
		return TicketClaim{}, err
	}
	return TicketClaim{VersionID: request.VersionID, TicketID: selected.IssueID, TicketNumber: selected.Number, TicketTitle: selected.Title, Owner: request.Owner, SessionID: sessionID, RunID: runID, Attempt: attempt, LeaseToken: leaseToken, LeaseGeneration: currentGeneration, LeaseExpiresAt: expiresAt}, nil
}

func (s *Store) CurrentClaim(ctx context.Context, versionID string, issueID int64) (TicketClaim, error) {
	var claim TicketClaim
	var expiresText string
	err := s.db.QueryRowContext(ctx, `SELECT s.session_id, s.owner, s.current_run_id, r.attempt, l.lease_token, l.generation, l.expires_at, t.issue_number, t.title
FROM ticket_sessions s
JOIN worker_runs r ON r.run_id = s.current_run_id
JOIN run_leases l ON l.run_id = r.run_id AND l.generation = r.lease_generation
JOIN plan_tickets t ON t.version_id = s.version_id AND t.issue_id = s.issue_id
WHERE s.version_id = ? AND s.issue_id = ? AND s.state = ? AND r.state = ?`, versionID, issueID, SessionRunning, RunRunning).
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

// MarkTicketDelivered records the durable delivery fact that unlocks dependent
// tickets. The caller is expected to have already verified the merged revision
// and reachability at the GitHub boundary.
func (s *Store) MarkTicketDelivered(ctx context.Context, versionID string, issueID int64) error {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := formatTimestamp(time.Now())
	result, err := tx.ExecContext(ctx, `INSERT INTO ticket_runtime(version_id, issue_id, state, delivered, updated_at)
SELECT version_id, issue_id, ?, 1, ? FROM plan_tickets WHERE version_id = ? AND issue_id = ?
ON CONFLICT(version_id, issue_id) DO UPDATE SET state = excluded.state, delivered = 1, updated_at = excluded.updated_at`, plan.StateDelivered, now, versionID, issueID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return ErrNotFound
	}
	var sessionID, runID string
	if err := tx.QueryRowContext(ctx, `SELECT session_id, COALESCE(current_run_id, '') FROM ticket_sessions WHERE version_id = ? AND issue_id = ?`, versionID, issueID).Scan(&sessionID, &runID); errors.Is(err, sql.ErrNoRows) {
		return tx.Commit()
	} else if err != nil {
		return err
	}
	if runID != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE worker_runs SET state = ?, finished_at = ? WHERE run_id = ? AND state = ?`, "succeeded", now, runID, RunRunning); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE run_leases SET state = ? WHERE run_id = ? AND state = ?`, "revoked", runID, LeaseActive); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ticket_sessions SET state = ?, owner = '', updated_at = ? WHERE session_id = ?`, SessionClosed, now, sessionID); err != nil {
		return err
	}
	return tx.Commit()
}

type frontierRows interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func loadFrontierTx(ctx context.Context, tx frontierRows, versionID string, now time.Time) (plan.FrontierSnapshot, error) {
	var snapshot plan.FrontierSnapshot
	var versionState, planState string
	err := tx.QueryRowContext(ctx, `SELECT v.state, p.state FROM plan_versions v JOIN plans p ON p.id = v.plan_id WHERE v.version_id = ?`, versionID).Scan(&versionState, &planState)
	if errors.Is(err, sql.ErrNoRows) {
		return snapshot, ErrNotFound
	}
	if err != nil {
		return snapshot, err
	}
	snapshot.VersionID = versionID
	if versionState == StateActive && planState == StateActive {
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
WHERE t.version_id = ?`, versionID)
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
		if runtimeState == plan.StateWaitingReview || runtimeState == plan.StateNeedsAttention {
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
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM worker_runs r
JOIN run_leases l ON l.run_id = r.run_id AND l.generation = r.lease_generation
WHERE r.state = ? AND l.state = ? AND l.expires_at > ?`, RunRunning, LeaseActive, formatTimestamp(now)).Scan(&snapshot.ActiveRuns); err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

func randomID(prefix string) (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(buffer), nil
}
