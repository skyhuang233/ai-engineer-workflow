package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/skyhuang233/workflow/internal/plan"
)

type ReviewFeedback struct {
	Source  string
	EventID string
	Author  string
	Body    string
}

func (s *Store) RecordReviewFeedback(ctx context.Context, versionID string, issueID int64, feedback []ReviewFeedback, now time.Time) (int, error) {
	if versionID == "" || issueID == 0 {
		return 0, ErrInvalidClaim
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM plan_tickets WHERE version_id = ? AND issue_id = ?`, versionID, issueID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, err
	}
	inserted := 0
	for _, event := range feedback {
		event.Source = strings.TrimSpace(event.Source)
		event.EventID = strings.TrimSpace(event.EventID)
		event.Author = strings.TrimSpace(event.Author)
		event.Body = strings.TrimSpace(event.Body)
		if event.Source == "" || event.EventID == "" || event.Body == "" {
			return 0, ErrInvalidClaim
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO review_feedback_events(version_id, issue_id, source, event_id, author, body, received_at)
VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT(version_id, issue_id, source, event_id) DO NOTHING`, versionID, issueID, event.Source, event.EventID, event.Author, event.Body, formatTimestamp(now))
		if err != nil {
			return 0, err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return 0, err
		}
		inserted += int(count)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return inserted, nil
}

func (s *Store) ClaimQueuedReviewRevision(ctx context.Context, versionID string, issueID int64, leaseTTL time.Duration, now time.Time, maxParallelRuns, maxAttempts int) (TicketClaim, string, error) {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	if versionID == "" || issueID == 0 || maxParallelRuns <= 0 {
		return TicketClaim{}, "", ErrInvalidClaim
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
		return TicketClaim{}, "", err
	}
	defer tx.Rollback()
	if frozen, err := planFrozenTx(ctx, tx, versionID); err != nil {
		return TicketClaim{}, "", err
	} else if frozen {
		return TicketClaim{}, "", ErrNotReady
	}
	var sessionID, owner, sessionState, runtimeState, currentRunID string
	var ticketNumber int64
	var ticketTitle string
	var generation, recoveryEpoch int64
	var consecutiveFailures int
	err = tx.QueryRowContext(ctx, `SELECT s.session_id, s.owner, s.state, s.current_run_id, s.current_lease_generation, rt.state, t.issue_number, t.title, s.consecutive_failures
FROM ticket_sessions s
JOIN ticket_runtime rt ON rt.version_id = s.version_id AND rt.issue_id = s.issue_id
JOIN plan_tickets t ON t.version_id = s.version_id AND t.issue_id = s.issue_id
	WHERE s.version_id = ? AND s.issue_id = ?`, versionID, issueID).Scan(&sessionID, &owner, &sessionState, &currentRunID, &generation, &runtimeState, &ticketNumber, &ticketTitle, &consecutiveFailures)
	if errors.Is(err, sql.ErrNoRows) {
		return TicketClaim{}, "", ErrNotFound
	}
	if err != nil {
		return TicketClaim{}, "", err
	}
	if currentRunID != "" {
		var runKind, runState, leaseState, expiresText string
		err := tx.QueryRowContext(ctx, `SELECT r.run_kind, r.state, l.state, l.expires_at
FROM worker_runs r JOIN run_leases l ON l.run_id = r.run_id AND l.generation = r.lease_generation
WHERE r.run_id = ?`, currentRunID).Scan(&runKind, &runState, &leaseState, &expiresText)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return TicketClaim{}, "", err
		}
		if err == nil && runState == RunRunning && leaseState == LeaseActive {
			expiresAt, parseErr := time.Parse(time.RFC3339Nano, expiresText)
			if parseErr != nil {
				return TicketClaim{}, "", parseErr
			}
			if expiresAt.After(now) {
				return TicketClaim{}, "", ErrNotReady
			}
			if runKind == RunDelivery {
				if _, err := tx.ExecContext(ctx, `UPDATE worker_runs SET state = 'failed', finished_at = ? WHERE run_id = ? AND state = ?`, formatTimestamp(now), currentRunID, RunRunning); err != nil {
					return TicketClaim{}, "", err
				}
				if _, err := tx.ExecContext(ctx, `UPDATE run_leases SET state = 'expired' WHERE run_id = ? AND state = ?`, currentRunID, LeaseActive); err != nil {
					return TicketClaim{}, "", err
				}
				if err := markTicketNeedsAttentionTx(ctx, tx, versionID, issueID, "Delivery Controller lease expired before completion", now); err != nil {
					return TicketClaim{}, "", err
				}
				if err := tx.Commit(); err != nil {
					return TicketClaim{}, "", err
				}
				return TicketClaim{}, "", ErrNeedsAttention
			}
			if _, err := tx.ExecContext(ctx, `UPDATE worker_runs SET state = 'superseded', finished_at = ? WHERE run_id = ? AND state = ?`, formatTimestamp(now), currentRunID, RunRunning); err != nil {
				return TicketClaim{}, "", err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE run_leases SET state = 'expired' WHERE run_id = ? AND state = ?`, currentRunID, LeaseActive); err != nil {
				return TicketClaim{}, "", err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE review_feedback_events SET claimed_run_id = '' WHERE claimed_run_id = ?`, currentRunID); err != nil {
				return TicketClaim{}, "", err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE ticket_sessions SET consecutive_failures = consecutive_failures + 1, updated_at = ? WHERE session_id = ?`, formatTimestamp(now), sessionID); err != nil {
				return TicketClaim{}, "", err
			}
			consecutiveFailures++
			if _, err := tx.ExecContext(ctx, `UPDATE ticket_runtime SET state = ?, updated_at = ? WHERE version_id = ? AND issue_id = ? AND delivered = 0`, plan.StateWaitingReview, formatTimestamp(now), versionID, issueID); err != nil {
				return TicketClaim{}, "", err
			}
			runtimeState = plan.StateWaitingReview
		}
	}
	if sessionState != SessionRunning || runtimeState != plan.StateWaitingReview || owner == "" {
		return TicketClaim{}, "", ErrNotReady
	}
	if err := tx.QueryRowContext(ctx, `SELECT recovery_epoch FROM ticket_sessions WHERE session_id = ?`, sessionID).Scan(&recoveryEpoch); err != nil {
		return TicketClaim{}, "", err
	}
	var activeRuns int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM worker_runs WHERE state = ? AND run_kind = ?`, RunRunning, RunAgent).Scan(&activeRuns); err != nil {
		return TicketClaim{}, "", err
	}
	if activeRuns >= maxParallelRuns {
		return TicketClaim{}, "", ErrCapacity
	}
	rows, err := tx.QueryContext(ctx, `SELECT source, event_id, author, body FROM review_feedback_events
WHERE version_id = ? AND issue_id = ? AND claimed_run_id = '' ORDER BY received_at, source, event_id`, versionID, issueID)
	if err != nil {
		return TicketClaim{}, "", err
	}
	defer rows.Close()
	var eventKeys []string
	var prompt strings.Builder
	for rows.Next() {
		var source, eventID, author, body string
		if err := rows.Scan(&source, &eventID, &author, &body); err != nil {
			return TicketClaim{}, "", err
		}
		eventKeys = append(eventKeys, source+"\x00"+eventID)
		if prompt.Len() > 0 {
			prompt.WriteString("\n\n")
		}
		if author == "" {
			fmt.Fprintf(&prompt, "Review feedback (%s):\n%s", source, body)
		} else {
			fmt.Fprintf(&prompt, "Review feedback from %s (%s):\n%s", author, source, body)
		}
	}
	if err := rows.Err(); err != nil {
		return TicketClaim{}, "", err
	}
	if err := rows.Close(); err != nil {
		return TicketClaim{}, "", err
	}
	if len(eventKeys) == 0 {
		return TicketClaim{}, "", ErrNotFound
	}
	generation++
	var attempt int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(attempt), 0) + 1 FROM worker_runs WHERE session_id = ?`, sessionID).Scan(&attempt); err != nil {
		return TicketClaim{}, "", err
	}
	limit := maxWorkerAttempts(maxAttempts)
	if consecutiveFailures >= limit {
		if err := markTicketNeedsAttentionTx(ctx, tx, versionID, issueID, "review revision retry budget exhausted", now); err != nil {
			return TicketClaim{}, "", err
		}
		if err := tx.Commit(); err != nil {
			return TicketClaim{}, "", err
		}
		return TicketClaim{}, "", ErrNotReady
	}
	runID, err := randomID("run-")
	if err != nil {
		return TicketClaim{}, "", err
	}
	leaseToken, err := randomID("lease-")
	if err != nil {
		return TicketClaim{}, "", err
	}
	expiresAt := now.Add(leaseTTL)
	if _, err := tx.ExecContext(ctx, `INSERT INTO worker_runs(run_id, session_id, attempt, recovery_epoch, lease_generation, state, started_at) VALUES (?, ?, ?, ?, ?, ?, ?)`, runID, sessionID, attempt, recoveryEpoch, generation, RunRunning, formatTimestamp(now)); err != nil {
		return TicketClaim{}, "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO run_leases(lease_token, run_id, session_id, generation, state, expires_at, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`, leaseToken, runID, sessionID, generation, LeaseActive, formatTimestamp(expiresAt), formatTimestamp(now)); err != nil {
		return TicketClaim{}, "", err
	}
	for _, key := range eventKeys {
		parts := strings.SplitN(key, "\x00", 2)
		if _, err := tx.ExecContext(ctx, `UPDATE review_feedback_events SET claimed_run_id = ? WHERE version_id = ? AND issue_id = ? AND source = ? AND event_id = ? AND claimed_run_id = ''`, runID, versionID, issueID, parts[0], parts[1]); err != nil {
			return TicketClaim{}, "", err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ticket_sessions SET current_run_id = ?, current_lease_generation = ?, updated_at = ? WHERE session_id = ?`, runID, generation, formatTimestamp(now), sessionID); err != nil {
		return TicketClaim{}, "", err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ticket_runtime SET state = ?, updated_at = ? WHERE version_id = ? AND issue_id = ? AND delivered = 0`, plan.StateRunning, formatTimestamp(now), versionID, issueID); err != nil {
		return TicketClaim{}, "", err
	}
	if err := tx.Commit(); err != nil {
		return TicketClaim{}, "", err
	}
	return TicketClaim{VersionID: versionID, TicketID: issueID, TicketNumber: ticketNumber, TicketTitle: ticketTitle, Owner: owner, SessionID: sessionID, RunID: runID, Attempt: attempt, LeaseToken: leaseToken, LeaseGeneration: generation, LeaseExpiresAt: expiresAt}, prompt.String(), nil
}

func (s *Store) FreezePlanForClosedPullRequest(ctx context.Context, versionID string, issueID int64, now time.Time) (bool, error) {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var delivered int
	err = tx.QueryRowContext(ctx, `SELECT delivered FROM ticket_runtime WHERE version_id = ? AND issue_id = ?`, versionID, issueID).Scan(&delivered)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	if delivered != 0 {
		return false, nil
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO plan_freezes(version_id, issue_id, reason, frozen_at) VALUES (?, ?, ?, ?)
ON CONFLICT(version_id) DO NOTHING`, versionID, issueID, "pull request closed without merge", formatTimestamp(now))
	if err != nil {
		return false, err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if inserted == 0 {
		return false, tx.Commit()
	}
	if err := markPlanNeedsAttentionTx(ctx, tx, versionID, "pull request closed without merge", now); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE worker_runs SET state = 'cancelled', finished_at = ? WHERE state = 'running' AND session_id IN (SELECT session_id FROM ticket_sessions WHERE version_id = ?)`, formatTimestamp(now), versionID); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE run_leases SET state = 'revoked' WHERE state = ? AND session_id IN (SELECT session_id FROM ticket_sessions WHERE version_id = ?)`, LeaseActive, versionID); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ticket_sessions SET owner = '', updated_at = ? WHERE version_id = ?`, formatTimestamp(now), versionID); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
