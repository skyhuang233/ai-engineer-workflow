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

func (s *Store) ClaimQueuedReviewRevision(ctx context.Context, versionID string, issueID int64, leaseTTL time.Duration, now time.Time, maxParallelRuns, maxAttempts int, provisionSession ...SessionProvisioner) (TicketClaim, string, error) {
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
	var sessionID, owner, sessionState, runtimeState, currentRunID, workspacePath, codexStatePath string
	var ticketNumber int64
	var ticketTitle string
	var generation, recoveryEpoch int64
	var consecutiveFailures int
	err = tx.QueryRowContext(ctx, `SELECT s.session_id, s.owner, s.state, s.current_run_id, s.current_lease_generation, rt.state, t.issue_number, t.title, s.consecutive_failures, s.workspace_path, s.codex_state_path
FROM ticket_sessions s
JOIN ticket_runtime rt ON rt.version_id = s.version_id AND rt.issue_id = s.issue_id
JOIN plan_tickets t ON t.version_id = s.version_id AND t.issue_id = s.issue_id
	WHERE s.version_id = ? AND s.issue_id = ?`, versionID, issueID).Scan(&sessionID, &owner, &sessionState, &currentRunID, &generation, &runtimeState, &ticketNumber, &ticketTitle, &consecutiveFailures, &workspacePath, &codexStatePath)
	if errors.Is(err, sql.ErrNoRows) {
		return TicketClaim{}, "", ErrNotFound
	}
	if err != nil {
		return TicketClaim{}, "", err
	}
	sessionProvisioned := false
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
			if len(provisionSession) > 0 && provisionSession[0] != nil {
				_, provisionErr := provisionSession[0](ctx, SessionProvisioning{SessionID: sessionID, Existing: true, WorkspacePath: workspacePath, CodexStatePath: codexStatePath, CurrentRunID: currentRunID})
				if provisionErr != nil {
					handled, failureErr := recordExpiredAuthenticationFailureTx(ctx, tx, versionID, issueID, currentRunID, provisionErr, now)
					if failureErr != nil {
						return TicketClaim{}, "", errors.Join(provisionErr, failureErr)
					}
					if handled {
						if err := tx.Commit(); err != nil {
							return TicketClaim{}, "", errors.Join(provisionErr, err)
						}
					}
					return TicketClaim{}, "", provisionErr
				}
				sessionProvisioned = true
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
	activeRuns, err := activeRunCountTx(ctx, tx, now)
	if err != nil {
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
	if !sessionProvisioned && len(provisionSession) > 0 && provisionSession[0] != nil {
		provisioning := SessionProvisioning{SessionID: sessionID, Existing: true, WorkspacePath: workspacePath, CodexStatePath: codexStatePath}
		if _, err := provisionSession[0](ctx, provisioning); err != nil {
			return TicketClaim{}, "", err
		}
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
	repository, report, err := closedUnmergedImpactReportTx(ctx, tx, versionID, issueID)
	if err != nil {
		return false, err
	}
	if err := ensureWorkflowQuestionTx(ctx, tx, repository, versionID, issueID, "closed_unmerged_impact", report, now); err != nil {
		return false, err
	}
	if err := recordClosedUnmergedQuestionContextTx(ctx, tx, repository, versionID, issueID); err != nil {
		return false, err
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

func recordClosedUnmergedQuestionContextTx(ctx context.Context, tx *sql.Tx, repository, versionID string, issueID int64) error {
	var questionID string
	if err := tx.QueryRowContext(ctx, `SELECT question_id FROM workflow_questions WHERE repository = ? AND version_id = ? AND issue_id = ? AND kind = 'closed_unmerged_impact' AND state = 'open'`, repository, versionID, issueID).Scan(&questionID); err != nil {
		return err
	}
	var ticketNumber, pullRequestNumber int64
	var commit, diagnostics, evidence string
	err := tx.QueryRowContext(ctx, `SELECT t.issue_number, COALESCE(td.pull_request_number, 0), COALESCE(s.accepted_commit, ''),
COALESCE((SELECT rd.diagnostics_path FROM run_diagnostics rd JOIN worker_runs r ON r.run_id = rd.run_id WHERE r.session_id = s.session_id ORDER BY rd.created_at DESC LIMIT 1), ''),
COALESCE((SELECT c.structured_output FROM candidate_revisions c WHERE c.session_id = s.session_id ORDER BY c.created_at DESC LIMIT 1), '')
FROM plan_tickets t
LEFT JOIN ticket_deliveries td ON td.version_id = t.version_id AND td.issue_id = t.issue_id
LEFT JOIN ticket_sessions s ON s.version_id = t.version_id AND s.issue_id = t.issue_id
WHERE t.version_id = ? AND t.issue_id = ?`, versionID, issueID).Scan(&ticketNumber, &pullRequestNumber, &commit, &diagnostics, &evidence)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO workflow_question_contexts(question_id, ticket_number, pull_request_number, accepted_commit, diagnostics_path, candidate_evidence)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(question_id) DO NOTHING`, questionID, ticketNumber, pullRequestNumber, commit, diagnostics, evidence)
	return err
}

func closedUnmergedImpactReportTx(ctx context.Context, tx *sql.Tx, versionID string, issueID int64) (string, string, error) {
	var repository string
	var rootNumber int64
	if err := tx.QueryRowContext(ctx, `SELECT p.repository, p.root_issue_number
FROM plan_versions v JOIN plans p ON p.id = v.plan_id WHERE v.version_id = ?`, versionID).Scan(&repository, &rootNumber); err != nil {
		return "", "", err
	}
	var report strings.Builder
	fmt.Fprintf(&report, "Pull request for ticket issue %d closed without merge. Plan #%d is frozen pending a human decision.\n\nTickets:\n", issueID, rootNumber)
	tickets, err := tx.QueryContext(ctx, `SELECT t.issue_number, t.title, COALESCE(rt.state, ?), COALESCE(rt.delivered, 0), COALESCE(s.agent_identity, ''), COALESCE(td.pull_request_number, 0)
FROM plan_tickets t
LEFT JOIN ticket_runtime rt ON rt.version_id = t.version_id AND rt.issue_id = t.issue_id
LEFT JOIN ticket_sessions s ON s.version_id = t.version_id AND s.issue_id = t.issue_id
LEFT JOIN ticket_deliveries td ON td.version_id = t.version_id AND td.issue_id = t.issue_id
WHERE t.version_id = ? ORDER BY t.issue_number`, plan.StateQueued, versionID)
	if err != nil {
		return "", "", err
	}
	defer tickets.Close()
	var merged []string
	for tickets.Next() {
		var number, pullRequest int64
		var title, state, agentIdentity string
		var delivered int
		if err := tickets.Scan(&number, &title, &state, &delivered, &agentIdentity, &pullRequest); err != nil {
			return "", "", err
		}
		if agentIdentity == "" {
			agentIdentity = "unassigned"
		}
		if pullRequest == 0 {
			fmt.Fprintf(&report, "- ticket #%d (%s): %s; agent %s; pull request none\n", number, title, state, agentIdentity)
		} else {
			fmt.Fprintf(&report, "- ticket #%d (%s): %s; agent %s; pull request #%d\n", number, title, state, agentIdentity, pullRequest)
		}
		if delivered != 0 {
			merged = append(merged, fmt.Sprintf("ticket #%d", number))
		}
	}
	if err := tickets.Err(); err != nil {
		return "", "", err
	}
	if len(merged) == 0 {
		report.WriteString("\nMerged work: none.\n")
	} else {
		fmt.Fprintf(&report, "\nMerged work: %s.\n", strings.Join(merged, ", "))
	}
	report.WriteString("\nCross-plan dependencies:\n")
	dependencies, err := tx.QueryContext(ctx, `SELECT p.repository, p.root_issue_number, t.issue_number
FROM plan_dependencies d
JOIN plan_tickets t ON t.version_id = d.version_id AND t.issue_id = d.blocked_issue_id
JOIN plan_versions v ON v.version_id = d.version_id
JOIN plans p ON p.id = v.plan_id AND p.current_version_id = v.version_id
WHERE d.blocker_issue_id = ? AND d.version_id <> ?
ORDER BY p.repository, p.root_issue_number, t.issue_number`, issueID, versionID)
	if err != nil {
		return "", "", err
	}
	defer dependencies.Close()
	foundDependency := false
	for dependencies.Next() {
		var dependentRepository string
		var dependentRoot, dependentTicket int64
		if err := dependencies.Scan(&dependentRepository, &dependentRoot, &dependentTicket); err != nil {
			return "", "", err
		}
		fmt.Fprintf(&report, "- %s plan #%d ticket #%d remains blocked for replanning\n", dependentRepository, dependentRoot, dependentTicket)
		foundDependency = true
	}
	if err := dependencies.Err(); err != nil {
		return "", "", err
	}
	if !foundDependency {
		report.WriteString("- none recorded\n")
	}
	report.WriteString("Reply with an id-addressed replacement or cancellation decision.")
	return repository, report.String(), nil
}
