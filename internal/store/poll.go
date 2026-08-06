package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/skyhuang233/workflow/internal/plan"
)

type WorkflowQuestion struct {
	ID           string
	Repository   string
	VersionID    string
	IssueID      int64
	Kind         string
	Prompt       string
	State        string
	Answer       string
	RootNumber   int64
	TicketNumber int64
	PullRequest  int64
	Commit       string
	Diagnostics  string
	Evidence     string
}

type closedPlanDecision struct {
	Action      string                `json:"action"`
	Replacement *replacementReference `json:"replacement,omitempty"`
}

type replacementReference struct {
	VersionID       string `json:"version_id,omitempty"`
	TicketID        int64  `json:"ticket_id,omitempty"`
	PlanRootIssueID int64  `json:"plan_root_issue_id,omitempty"`
}

func (s *Store) OpenWorkflowQuestions(ctx context.Context, repository string, rootNumber int64) ([]WorkflowQuestion, error) {
	return openWorkflowQuestions(ctx, s.db, repository, rootNumber)
}

func (s *Store) WorkflowInboxProjection(ctx context.Context, repository string) ([]WorkflowQuestion, string, error) {
	questions, err := s.OpenWorkflowQuestions(ctx, repository, 0)
	if err != nil {
		return nil, "", err
	}
	version, err := workflowInboxProjectionVersion(questions)
	if err != nil {
		return nil, "", err
	}
	return questions, version, nil
}

type workflowQuestionQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func openWorkflowQuestions(ctx context.Context, querier workflowQuestionQuerier, repository string, rootNumber int64) ([]WorkflowQuestion, error) {
	rows, err := querier.QueryContext(ctx, `SELECT q.question_id, q.repository, q.version_id, q.issue_id, q.kind, q.prompt, q.state, q.answer, p.root_issue_number,
COALESCE(qc.ticket_number, t.issue_number, 0), COALESCE(qc.pull_request_number, d.pull_request_number, 0), COALESCE(qc.accepted_commit, s.accepted_commit, ''), COALESCE(qc.diagnostics_path, rd.diagnostics_path, ''), COALESCE(qc.candidate_evidence, c.structured_output, '')
FROM workflow_questions q
JOIN plan_versions v ON v.version_id = q.version_id
JOIN plans p ON p.id = v.plan_id
LEFT JOIN workflow_question_contexts qc ON qc.question_id = q.question_id
LEFT JOIN plan_tickets t ON t.version_id = q.version_id AND t.issue_id = q.issue_id
LEFT JOIN ticket_deliveries d ON d.version_id = q.version_id AND d.issue_id = q.issue_id
LEFT JOIN ticket_sessions s ON s.version_id = q.version_id AND s.issue_id = q.issue_id
LEFT JOIN run_diagnostics rd ON rd.run_id = s.current_run_id
LEFT JOIN candidate_revisions c ON c.run_id = s.current_run_id
WHERE q.repository = ? AND (? = 0 OR p.root_issue_number = ?) AND q.state = 'open' ORDER BY q.created_at, q.question_id`, repository, rootNumber, rootNumber)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var questions []WorkflowQuestion
	for rows.Next() {
		var question WorkflowQuestion
		if err := rows.Scan(&question.ID, &question.Repository, &question.VersionID, &question.IssueID, &question.Kind, &question.Prompt, &question.State, &question.Answer, &question.RootNumber, &question.TicketNumber, &question.PullRequest, &question.Commit, &question.Diagnostics, &question.Evidence); err != nil {
			return nil, err
		}
		questions = append(questions, question)
	}
	return questions, rows.Err()
}

type GitHubPollCursor struct {
	Repository          string
	LastSuccessAt       time.Time
	LastFullReconcileAt time.Time
	ConsecutiveFailures int
	FailureKind         GitHubPollFailureKind
	RecoveryState       GitHubPollRecoveryState
	NextAttemptAt       time.Time
}

func (c GitHubPollCursor) NeedsAttention() bool {
	return c.FailureKind == GitHubPollFailureUnrecoverable && c.RecoveryState == GitHubPollRecoveryConsumed
}

type GitHubPollFailureKind string
type GitHubPollRecoveryState string

const (
	GitHubPollFailurePreActivationInboxConflict GitHubPollFailureKind   = "pre_activation_inbox_conflict"
	GitHubPollFailureUnrecoverable              GitHubPollFailureKind   = "unrecoverable"
	GitHubPollRecoveryAvailable                 GitHubPollRecoveryState = "available"
	GitHubPollRecoveryClaimed                   GitHubPollRecoveryState = "claimed"
	GitHubPollRecoveryCompleted                 GitHubPollRecoveryState = "completed"
	GitHubPollRecoveryConsumed                  GitHubPollRecoveryState = "consumed"
)

func (s *Store) GitHubPollCursor(ctx context.Context, repository string) (GitHubPollCursor, error) {
	return scanGitHubPollCursor(s.db.QueryRowContext(ctx, `SELECT repository, last_success_at, last_full_reconcile_at, consecutive_failures, failure_kind, recovery_state, next_attempt_at
FROM github_poll_cursors WHERE repository = ?`, repository))
}

type githubPollCursorScanner interface {
	Scan(...any) error
}

func scanGitHubPollCursor(row githubPollCursorScanner) (GitHubPollCursor, error) {
	var cursor GitHubPollCursor
	var success, full, next string
	err := row.Scan(&cursor.Repository, &success, &full, &cursor.ConsecutiveFailures, &cursor.FailureKind, &cursor.RecoveryState, &next)
	if errors.Is(err, sql.ErrNoRows) {
		return GitHubPollCursor{}, ErrNotFound
	}
	if err != nil {
		return GitHubPollCursor{}, err
	}
	var parseErr error
	if success != "" {
		cursor.LastSuccessAt, parseErr = time.Parse(time.RFC3339Nano, success)
		if parseErr != nil {
			return GitHubPollCursor{}, parseErr
		}
	}
	if full != "" {
		cursor.LastFullReconcileAt, parseErr = time.Parse(time.RFC3339Nano, full)
		if parseErr != nil {
			return GitHubPollCursor{}, parseErr
		}
	}
	if next != "" {
		cursor.NextAttemptAt, parseErr = time.Parse(time.RFC3339Nano, next)
		if parseErr != nil {
			return GitHubPollCursor{}, parseErr
		}
	}
	return cursor, nil
}

func (s *Store) HasActiveDeliveryPlan(ctx context.Context, repository string) (bool, error) {
	var active int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM plans p JOIN plan_versions v ON v.version_id = p.current_version_id
WHERE p.repository = ? AND `+currentActivePlanPredicate+` LIMIT 1`, repository).Scan(&active)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) ClaimGitHubPollBootstrapRecovery(ctx context.Context, repository string, minimumFailures int, now time.Time) (bool, error) {
	if minimumFailures < 1 {
		return false, ErrInvalidClaim
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	result, err := s.db.ExecContext(ctx, `UPDATE github_poll_cursors
SET recovery_state = ?, updated_at = ?
WHERE repository = ? AND consecutive_failures >= ? AND failure_kind = ? AND recovery_state = ?`, GitHubPollRecoveryClaimed, formatTimestamp(now), repository, minimumFailures, GitHubPollFailurePreActivationInboxConflict, GitHubPollRecoveryAvailable)
	if err != nil {
		return false, err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return updated == 1, nil
}

func (s *Store) RecoverGitHubPollAfterBootstrap(ctx context.Context, repository string, now time.Time) (bool, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	result, err := s.db.ExecContext(ctx, `UPDATE github_poll_cursors
SET consecutive_failures = 0, failure_kind = '', recovery_state = ?, next_attempt_at = ?, updated_at = ?
WHERE repository = ? AND failure_kind = ? AND recovery_state = ?`, GitHubPollRecoveryCompleted, formatTimestamp(now), formatTimestamp(now), repository, GitHubPollFailurePreActivationInboxConflict, GitHubPollRecoveryClaimed)
	if err != nil {
		return false, err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return updated == 1, nil
}

func (s *Store) MarkGitHubPollFailureUnrecoverable(ctx context.Context, repository string, now time.Time) error {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO github_poll_cursors(repository, consecutive_failures, failure_kind, recovery_state, next_attempt_at, updated_at)
VALUES (?, 0, ?, ?, ?, ?)
ON CONFLICT(repository) DO UPDATE SET failure_kind = excluded.failure_kind, recovery_state = excluded.recovery_state, updated_at = excluded.updated_at`, repository, GitHubPollFailureUnrecoverable, GitHubPollRecoveryConsumed, formatTimestamp(now), formatTimestamp(now))
	return err
}

func (s *Store) RecordGitHubPollSuccess(ctx context.Context, repository string, now time.Time, fullReconcile bool) error {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	full := ""
	if fullReconcile {
		full = formatTimestamp(now)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO github_poll_cursors(repository, last_success_at, last_full_reconcile_at, consecutive_failures, failure_kind, recovery_state, next_attempt_at, updated_at)
VALUES (?, ?, ?, 0, '', '', ?, ?)
ON CONFLICT(repository) DO UPDATE SET last_success_at = excluded.last_success_at,
last_full_reconcile_at = CASE WHEN excluded.last_full_reconcile_at = '' THEN github_poll_cursors.last_full_reconcile_at ELSE excluded.last_full_reconcile_at END,
consecutive_failures = 0, failure_kind = '',
recovery_state = CASE WHEN github_poll_cursors.recovery_state = 'completed' THEN github_poll_cursors.recovery_state ELSE '' END,
next_attempt_at = excluded.next_attempt_at, updated_at = excluded.updated_at`, repository, formatTimestamp(now), full, formatTimestamp(now), formatTimestamp(now))
	return err
}

func (s *Store) DeferGitHubPoll(ctx context.Context, repository string, retryAt, now time.Time) error {
	_, err := s.DeferGitHubPollWithCursor(ctx, repository, retryAt, now)
	return err
}

func (s *Store) DeferGitHubPollWithCursor(ctx context.Context, repository string, retryAt, now time.Time) (GitHubPollCursor, error) {
	if retryAt.IsZero() {
		return GitHubPollCursor{}, ErrInvalidClaim
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	return scanGitHubPollCursor(s.db.QueryRowContext(ctx, `INSERT INTO github_poll_cursors(repository, consecutive_failures, failure_kind, recovery_state, next_attempt_at, updated_at)
VALUES (?, 0, '', '', ?, ?)
ON CONFLICT(repository) DO UPDATE SET
failure_kind = CASE WHEN github_poll_cursors.failure_kind = ? OR github_poll_cursors.recovery_state IN (?, ?) THEN ? ELSE github_poll_cursors.failure_kind END,
recovery_state = CASE WHEN github_poll_cursors.failure_kind = ? OR github_poll_cursors.recovery_state IN (?, ?) THEN ? ELSE github_poll_cursors.recovery_state END,
next_attempt_at = CASE WHEN github_poll_cursors.next_attempt_at > excluded.next_attempt_at THEN github_poll_cursors.next_attempt_at ELSE excluded.next_attempt_at END,
updated_at = excluded.updated_at
RETURNING repository, last_success_at, last_full_reconcile_at, consecutive_failures, failure_kind, recovery_state, next_attempt_at`, repository, formatTimestamp(retryAt), formatTimestamp(now), GitHubPollFailurePreActivationInboxConflict, GitHubPollRecoveryAvailable, GitHubPollRecoveryClaimed, GitHubPollFailureUnrecoverable, GitHubPollFailurePreActivationInboxConflict, GitHubPollRecoveryAvailable, GitHubPollRecoveryClaimed, GitHubPollRecoveryConsumed))
}

func (s *Store) MarkRepositoryNeedsAttention(ctx context.Context, repository string, now time.Time) error {
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
	rows, err := tx.QueryContext(ctx, `SELECT v.version_id FROM plans p JOIN plan_versions v ON v.version_id = p.current_version_id WHERE p.repository = ? AND `+currentActiveUnfrozenPlanPredicate, repository)
	if err != nil {
		return err
	}
	var versionIDs []string
	for rows.Next() {
		var versionID string
		if err := rows.Scan(&versionID); err != nil {
			rows.Close()
			return err
		}
		versionIDs = append(versionIDs, versionID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, versionID := range versionIDs {
		if err := ensureWorkflowQuestionTx(ctx, tx, repository, versionID, 0, "poll_failure", "GitHub polling exhausted its retry budget. Reply with an id-addressed retry decision after resolving the GitHub access failure.", now); err != nil {
			return err
		}
		var pollQuestionID string
		if err := tx.QueryRowContext(ctx, `SELECT question_id FROM workflow_questions WHERE repository = ? AND version_id = ? AND issue_id = 0 AND kind = 'poll_failure' AND state = 'open'`, repository, versionID).Scan(&pollQuestionID); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT issue_id FROM ticket_runtime WHERE version_id = ? AND delivered = 0 AND state != ?`, versionID, plan.StateNeedsAttention)
		if err != nil {
			return err
		}
		var issueIDs []int64
		for rows.Next() {
			var issueID int64
			if err := rows.Scan(&issueID); err != nil {
				rows.Close()
				return err
			}
			issueIDs = append(issueIDs, issueID)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, issueID := range issueIDs {
			if err := markTicketNeedsAttentionTx(ctx, tx, versionID, issueID, "GitHub polling exhausted its retry budget", now); err != nil {
				return err
			}
			var ticketQuestionID string
			if err := tx.QueryRowContext(ctx, `SELECT question_id FROM workflow_questions WHERE repository = ? AND version_id = ? AND issue_id = ? AND kind = 'needs_attention' AND state = 'open'`, repository, versionID, issueID).Scan(&ticketQuestionID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO poll_failure_targets(poll_question_id, version_id, issue_id, ticket_question_id) VALUES (?, ?, ?, ?)
ON CONFLICT(poll_question_id, version_id, issue_id) DO NOTHING`, pollQuestionID, versionID, issueID, ticketQuestionID); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (s *Store) WorkflowQuestion(ctx context.Context, repository, questionID string) (WorkflowQuestion, error) {
	var question WorkflowQuestion
	err := s.db.QueryRowContext(ctx, `SELECT q.question_id, q.repository, q.version_id, q.issue_id, q.kind, q.prompt, q.state, q.answer, p.root_issue_number,
COALESCE(qc.ticket_number, t.issue_number, 0), COALESCE(qc.pull_request_number, d.pull_request_number, 0), COALESCE(qc.accepted_commit, s.accepted_commit, ''), COALESCE(qc.diagnostics_path, rd.diagnostics_path, ''), COALESCE(qc.candidate_evidence, c.structured_output, '')
FROM workflow_questions q
JOIN plan_versions v ON v.version_id = q.version_id
JOIN plans p ON p.id = v.plan_id
LEFT JOIN workflow_question_contexts qc ON qc.question_id = q.question_id
LEFT JOIN plan_tickets t ON t.version_id = q.version_id AND t.issue_id = q.issue_id
LEFT JOIN ticket_deliveries d ON d.version_id = q.version_id AND d.issue_id = q.issue_id
LEFT JOIN ticket_sessions s ON s.version_id = q.version_id AND s.issue_id = q.issue_id
LEFT JOIN run_diagnostics rd ON rd.run_id = s.current_run_id
LEFT JOIN candidate_revisions c ON c.run_id = s.current_run_id
WHERE q.question_id = ? AND q.repository = ?`, questionID, repository).
		Scan(&question.ID, &question.Repository, &question.VersionID, &question.IssueID, &question.Kind, &question.Prompt, &question.State, &question.Answer, &question.RootNumber, &question.TicketNumber, &question.PullRequest, &question.Commit, &question.Diagnostics, &question.Evidence)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkflowQuestion{}, ErrNotFound
	}
	return question, err
}

func (s *Store) AnswerWorkflowQuestion(ctx context.Context, repository, questionID, answer string, now time.Time) error {
	if repository == "" || questionID == "" || answer == "" {
		return ErrInvalidClaim
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.answerWorkflowQuestionTx(ctx, tx, repository, questionID, answer, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) AnswerWorkflowQuestionAndQueueInboxProjection(ctx context.Context, repository, questionID, answer string, now time.Time) (DeliveryOutbox, error) {
	if repository == "" || questionID == "" || answer == "" {
		return DeliveryOutbox{}, ErrInvalidClaim
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DeliveryOutbox{}, err
	}
	defer tx.Rollback()
	if err := s.answerWorkflowQuestionTx(ctx, tx, repository, questionID, answer, now); err != nil {
		return DeliveryOutbox{}, err
	}
	outbox, err := s.queueWorkflowInboxProjectionTx(ctx, tx, repository, now)
	if err != nil {
		return DeliveryOutbox{}, err
	}
	if err := tx.Commit(); err != nil {
		return DeliveryOutbox{}, err
	}
	return outbox, nil
}

func (s *Store) QueueWorkflowInboxProjection(ctx context.Context, repository string, now time.Time) (DeliveryOutbox, error) {
	if repository == "" {
		return DeliveryOutbox{}, ErrInvalidClaim
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DeliveryOutbox{}, err
	}
	defer tx.Rollback()
	outbox, err := s.queueWorkflowInboxProjectionTx(ctx, tx, repository, now)
	if err != nil {
		return DeliveryOutbox{}, err
	}
	if err := tx.Commit(); err != nil {
		return DeliveryOutbox{}, err
	}
	return outbox, nil
}

func (s *Store) queueWorkflowInboxProjectionTx(ctx context.Context, tx *sql.Tx, repository string, now time.Time) (DeliveryOutbox, error) {
	questions, err := openWorkflowQuestions(ctx, tx, repository, 0)
	if err != nil {
		return DeliveryOutbox{}, err
	}
	version, err := workflowInboxProjectionVersion(questions)
	if err != nil {
		return DeliveryOutbox{}, err
	}
	return s.enqueueDeliveryTx(ctx, tx, DeliveryRequest{Operation: DeliveryProjectInbox, Repository: repository, InboxProjectionVersion: version}, now)
}

func workflowInboxProjectionVersion(questions []WorkflowQuestion) (string, error) {
	projected := make([]plan.WorkflowQuestion, 0, len(questions))
	for _, question := range questions {
		projected = append(projected, plan.WorkflowQuestion{ID: question.ID, Prompt: question.Prompt, Repository: question.Repository, PlanNumber: question.RootNumber, TicketNumber: question.TicketNumber, PullRequest: question.PullRequest, Commit: question.Commit, Finding: question.Kind, Diagnostics: question.Diagnostics, Evidence: question.Evidence})
	}
	encoded, err := json.Marshal(projected)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func (s *Store) answerWorkflowQuestionTx(ctx context.Context, tx *sql.Tx, repository, questionID, answer string, now time.Time) error {
	var kind, versionID, state, priorAnswer string
	var issueID int64
	err := tx.QueryRowContext(ctx, `SELECT kind, version_id, issue_id, state, COALESCE(answer, '') FROM workflow_questions WHERE question_id = ? AND repository = ?`, questionID, repository).Scan(&kind, &versionID, &issueID, &state, &priorAnswer)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if state != "open" {
		if kind == "closed_unmerged_impact" && state == "answered" && priorAnswer == answer {
			return nil
		}
		return ErrNotFound
	}
	if kind == "closed_unmerged_impact" {
		var decision closedPlanDecision
		if err := json.Unmarshal([]byte(answer), &decision); err != nil {
			return ErrInvalidClaim
		}
		switch decision.Action {
		case "cancel-plan":
			if decision.Replacement != nil {
				return ErrInvalidClaim
			}
			if err := cancelPlanTx(ctx, tx, versionID, now); err != nil {
				return err
			}
		case "replace":
			if decision.Replacement == nil {
				return ErrInvalidClaim
			}
			if err := replaceFrozenPlanTx(ctx, tx, repository, questionID, versionID, issueID, *decision.Replacement, now); err != nil {
				return err
			}
		default:
			return ErrInvalidClaim
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workflow_questions SET state = 'answered', answer = ?, answered_at = ? WHERE question_id = ?`, answer, formatTimestamp(now), questionID); err != nil {
		return err
	}
	if kind == "poll_failure" {
		if _, err := tx.ExecContext(ctx, `UPDATE github_poll_cursors SET consecutive_failures = 0, failure_kind = '', recovery_state = '', next_attempt_at = ?, updated_at = ? WHERE repository = ?`, formatTimestamp(now), formatTimestamp(now), repository); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT t.issue_id, COALESCE(s.session_id, ''), COALESCE(s.accepted_commit, '')
FROM poll_failure_targets t
LEFT JOIN ticket_sessions s ON s.version_id = t.version_id AND s.issue_id = t.issue_id
WHERE t.poll_question_id = ? AND t.version_id = ?`, questionID, versionID)
		if err != nil {
			return err
		}
		type recoveryTarget struct {
			issueID        int64
			sessionID      string
			acceptedCommit string
		}
		var targets []recoveryTarget
		for rows.Next() {
			var targetIssueID int64
			var sessionID, acceptedCommit string
			if err := rows.Scan(&targetIssueID, &sessionID, &acceptedCommit); err != nil {
				rows.Close()
				return err
			}
			targets = append(targets, recoveryTarget{issueID: targetIssueID, sessionID: sessionID, acceptedCommit: acceptedCommit})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, target := range targets {
			if err := recoverNeedsAttentionTicketTx(ctx, tx, versionID, target.issueID, target.sessionID, target.acceptedCommit, now); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE workflow_questions SET state = 'answered', answer = ?, answered_at = ? WHERE state = 'open' AND question_id IN (
    SELECT ticket_question_id FROM poll_failure_targets WHERE poll_question_id = ?
)`, "resolved by poll retry", formatTimestamp(now), questionID); err != nil {
			return err
		}
	} else if kind == "needs_attention" {
		var sessionID, acceptedCommit string
		err := tx.QueryRowContext(ctx, `SELECT session_id, accepted_commit FROM ticket_sessions WHERE version_id = ? AND issue_id = ?`, versionID, issueID).Scan(&sessionID, &acceptedCommit)
		if errors.Is(err, sql.ErrNoRows) {
			sessionID = ""
			acceptedCommit = ""
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err := recoverNeedsAttentionTicketTx(ctx, tx, versionID, issueID, sessionID, acceptedCommit, now); err != nil {
			return err
		}
	}
	return nil
}

func cancelPlanTx(ctx context.Context, tx *sql.Tx, versionID string, now time.Time) error {
	stamp := formatTimestamp(now)
	if _, err := tx.ExecContext(ctx, `UPDATE ticket_runtime SET state = ?, updated_at = ? WHERE version_id = ? AND delivered = 0`, plan.StateCancelled, stamp, versionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE worker_runs SET state = 'cancelled', finished_at = ? WHERE state = ? AND session_id IN (SELECT session_id FROM ticket_sessions WHERE version_id = ?)`, stamp, RunRunning, versionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE run_leases SET state = 'revoked' WHERE state = ? AND session_id IN (SELECT session_id FROM ticket_sessions WHERE version_id = ?)`, LeaseActive, versionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ticket_sessions SET state = ?, owner = '', updated_at = ? WHERE version_id = ?`, SessionClosed, stamp, versionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO plan_terminal_states(version_id, state, recorded_at) VALUES (?, ?, ?) ON CONFLICT(version_id) DO NOTHING`, versionID, "cancelled", stamp); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM plan_freezes WHERE version_id = ?`, versionID)
	return err
}

func replaceFrozenPlanTx(ctx context.Context, tx *sql.Tx, repository, questionID, versionID string, questionIssueID int64, replacement replacementReference, now time.Time) error {
	var frozenIssueID int64
	if err := tx.QueryRowContext(ctx, `SELECT issue_id FROM plan_freezes WHERE version_id = ?`, versionID).Scan(&frozenIssueID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotReady
		}
		return err
	}
	if questionIssueID != frozenIssueID {
		return ErrNotReady
	}
	if (replacement.VersionID == "" || replacement.TicketID == 0) == (replacement.PlanRootIssueID == 0) {
		return ErrInvalidClaim
	}
	var replacementVersionID string
	var replacementIssueID int64
	if replacement.PlanRootIssueID != 0 {
		err := tx.QueryRowContext(ctx, `SELECT v.version_id
FROM plans p
JOIN plan_versions v ON v.version_id = p.current_version_id
WHERE p.repository = ? AND p.root_issue_id = ? AND `+currentSchedulableReplacementPlanPredicate, repository, replacement.PlanRootIssueID).Scan(&replacementVersionID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotReady
		}
		if err != nil {
			return err
		}
	} else {
		err := tx.QueryRowContext(ctx, `SELECT v.version_id
FROM plan_tickets t
JOIN ticket_runtime r ON r.version_id = t.version_id AND r.issue_id = t.issue_id
JOIN plan_versions v ON v.version_id = t.version_id
JOIN plans p ON p.id = v.plan_id
WHERE t.version_id = ? AND t.issue_id = ? AND p.repository = ? AND r.delivered = 0 AND r.state != ? AND `+currentActiveUnfrozenPlanPredicate,
			replacement.VersionID, replacement.TicketID, repository, plan.StateCancelled).Scan(&replacementVersionID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotReady
		}
		if err != nil {
			return err
		}
		replacementIssueID = replacement.TicketID
	}
	if replacementVersionID == versionID {
		return ErrInvalidClaim
	}
	replacementJSON, err := json.Marshal(replacement)
	if err != nil {
		return err
	}
	stamp := formatTimestamp(now)
	if _, err := tx.ExecContext(ctx, `INSERT INTO replacement_tickets(question_id, version_id, retired_issue_id, replacement, replacement_version_id, replacement_issue_id, state, approved_at)
VALUES (?, ?, ?, ?, ?, ?, 'approved', ?)`, questionID, versionID, frozenIssueID, string(replacementJSON), replacementVersionID, replacementIssueID, stamp); err != nil {
		return err
	}
	return nil
}

func (s *Store) SchedulerRoot(ctx context.Context, repository string, configuredRoot int64, now time.Time) (int64, error) {
	if repository == "" || configuredRoot <= 0 {
		return 0, ErrInvalidClaim
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var sourceVersionID string
	err = tx.QueryRowContext(ctx, `SELECT p.current_version_id FROM plans p WHERE p.repository = ? AND p.root_issue_number = ?`, repository, configuredRoot).Scan(&sourceVersionID)
	if errors.Is(err, sql.ErrNoRows) {
		if err := tx.Commit(); err != nil {
			return 0, err
		}
		return configuredRoot, nil
	}
	if err != nil {
		return 0, err
	}
	var handoffState, targetVersionID string
	err = tx.QueryRowContext(ctx, `SELECT state, replacement_version_id FROM replacement_tickets
WHERE version_id = ? AND state IN ('approved', 'active') ORDER BY approved_at DESC LIMIT 1`, sourceVersionID).Scan(&handoffState, &targetVersionID)
	if errors.Is(err, sql.ErrNoRows) {
		if err := tx.Commit(); err != nil {
			return 0, err
		}
		return configuredRoot, nil
	}
	if err != nil {
		return 0, err
	}
	var targetRoot int64
	var targetVersionState, targetPlanState string
	err = tx.QueryRowContext(ctx, `SELECT p.root_issue_number,
COALESCE((SELECT terminal.state FROM plan_terminal_states terminal WHERE terminal.version_id = v.version_id), CASE WHEN EXISTS (SELECT 1 FROM completed_plan_versions completed WHERE completed.version_id = v.version_id) THEN ? ELSE v.state END), p.state
FROM plan_versions v JOIN plans p ON p.id = v.plan_id
WHERE v.version_id = ? AND p.repository = ? AND p.current_version_id = v.version_id`, StateCompleted, targetVersionID, repository).Scan(&targetRoot, &targetVersionState, &targetPlanState)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotReady
	}
	if err != nil {
		return 0, err
	}
	var frozen int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM plan_freezes WHERE version_id = ?`, targetVersionID).Scan(&frozen)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	targetFrozen := err == nil
	if !targetFrozen && handoffState == "approved" && targetVersionState == StateProjecting && targetPlanState == StateProjecting {
		if err := tx.Commit(); err != nil {
			return 0, err
		}
		return targetRoot, nil
	}
	targetActivated := targetPlanState == StateActive && (targetVersionState == StateActive || targetVersionState == StateCompleted)
	if targetFrozen || !targetActivated {
		return 0, ErrNotReady
	}
	if handoffState == "approved" {
		if err := cancelPlanTx(ctx, tx, sourceVersionID, now); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE replacement_tickets SET state = 'active' WHERE version_id = ? AND state = 'approved'`, sourceVersionID); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return targetRoot, nil
}

func recoverNeedsAttentionTicketTx(ctx context.Context, tx *sql.Tx, versionID string, issueID int64, sessionID, acceptedCommit string, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM plan_freezes WHERE version_id = ? AND issue_id = ?`, versionID, issueID); err != nil {
		return err
	}
	if acceptedCommit != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE ticket_sessions SET consecutive_failures = 0, delivery_retry_pending = 1, updated_at = ? WHERE session_id = ?`, formatTimestamp(now), sessionID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE ticket_runtime SET state = ?, updated_at = ? WHERE version_id = ? AND issue_id = ? AND delivered = 0`, plan.StateWaitingReview, formatTimestamp(now), versionID, issueID)
		return err
	}
	if sessionID != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE ticket_sessions SET consecutive_failures = 0, recovery_epoch = recovery_epoch + 1, updated_at = ? WHERE session_id = ?`, formatTimestamp(now), sessionID); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `UPDATE ticket_runtime SET state = ?, updated_at = ? WHERE version_id = ? AND issue_id = ? AND delivered = 0`, plan.StateQueued, formatTimestamp(now), versionID, issueID)
	return err
}

func ensureWorkflowQuestionTx(ctx context.Context, tx *sql.Tx, repository, versionID string, issueID int64, kind, prompt string, now time.Time) error {
	var open int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM workflow_questions WHERE repository = ? AND version_id = ? AND issue_id = ? AND kind = ? AND state = 'open'`, repository, versionID, issueID, kind).Scan(&open); err != nil {
		return err
	}
	if open != 0 {
		return nil
	}
	var generation int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(generation), 0) + 1 FROM workflow_questions WHERE repository = ? AND version_id = ? AND issue_id = ? AND kind = ?`, repository, versionID, issueID, kind).Scan(&generation); err != nil {
		return err
	}
	questionID := fmt.Sprintf("%s-%s-%d-g%d", strings.ReplaceAll(kind, "_", "-"), versionID, issueID, generation)
	_, err := tx.ExecContext(ctx, `INSERT INTO workflow_questions(question_id, repository, version_id, issue_id, kind, generation, prompt, state, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, 'open', ?)
ON CONFLICT(repository, version_id, issue_id, kind, generation) DO NOTHING`, questionID, repository, versionID, issueID, kind, generation, prompt, formatTimestamp(now))
	return err
}

func markTicketNeedsAttentionTx(ctx context.Context, tx *sql.Tx, versionID string, issueID int64, reason string, now time.Time) error {
	var repository string
	var number int64
	err := tx.QueryRowContext(ctx, `SELECT p.repository, t.issue_number
FROM plan_tickets t
JOIN plan_versions v ON v.version_id = t.version_id
JOIN plans p ON p.id = v.plan_id
WHERE t.version_id = ? AND t.issue_id = ?`, versionID, issueID).Scan(&repository, &number)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ticket_runtime SET state = ?, updated_at = ? WHERE version_id = ? AND issue_id = ? AND delivered = 0`, plan.StateNeedsAttention, formatTimestamp(now), versionID, issueID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ticket_sessions SET delivery_retry_pending = 0, updated_at = ? WHERE version_id = ? AND issue_id = ?`, formatTimestamp(now), versionID, issueID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE review_feedback_events SET claimed_run_id = '' WHERE claimed_run_id IN (
    SELECT run_id FROM worker_runs WHERE state = ? AND session_id = (SELECT session_id FROM ticket_sessions WHERE version_id = ? AND issue_id = ?)
)`, RunRunning, versionID, issueID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE worker_runs SET state = 'cancelled', finished_at = ? WHERE state = ? AND session_id = (SELECT session_id FROM ticket_sessions WHERE version_id = ? AND issue_id = ?)`, formatTimestamp(now), RunRunning, versionID, issueID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE run_leases SET state = 'revoked' WHERE state = ? AND session_id = (SELECT session_id FROM ticket_sessions WHERE version_id = ? AND issue_id = ?)`, LeaseActive, versionID, issueID); err != nil {
		return err
	}
	prompt := fmt.Sprintf("Ticket #%d needs attention: %s Reply with an id-addressed retry decision after resolving the cause.", number, reason)
	return ensureWorkflowQuestionTx(ctx, tx, repository, versionID, issueID, "needs_attention", prompt, now)
}

func markPlanNeedsAttentionTx(ctx context.Context, tx *sql.Tx, versionID, reason string, now time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT issue_id FROM ticket_runtime WHERE version_id = ? AND delivered = 0`, versionID)
	if err != nil {
		return err
	}
	var issueIDs []int64
	for rows.Next() {
		var issueID int64
		if err := rows.Scan(&issueID); err != nil {
			rows.Close()
			return err
		}
		issueIDs = append(issueIDs, issueID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, issueID := range issueIDs {
		if err := markTicketNeedsAttentionTx(ctx, tx, versionID, issueID, reason, now); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) RecordGitHubPollFailure(ctx context.Context, repository string, now time.Time) error {
	_, err := s.AdvanceGitHubPollFailure(ctx, repository, now, "")
	return err
}

func (s *Store) RecordGitHubPollFailureWithKind(ctx context.Context, repository string, now time.Time, kind GitHubPollFailureKind) error {
	_, err := s.AdvanceGitHubPollFailure(ctx, repository, now, kind)
	return err
}

func (s *Store) AdvanceGitHubPollFailure(ctx context.Context, repository string, now time.Time, kind GitHubPollFailureKind) (GitHubPollCursor, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return GitHubPollCursor{}, err
	}
	defer tx.Rollback()
	var failures int
	var existingKind GitHubPollFailureKind
	var existingRecovery GitHubPollRecoveryState
	err = tx.QueryRowContext(ctx, `SELECT consecutive_failures, failure_kind, recovery_state FROM github_poll_cursors WHERE repository = ?`, repository).Scan(&failures, &existingKind, &existingRecovery)
	if errors.Is(err, sql.ErrNoRows) {
		failures = 0
	} else if err != nil {
		return GitHubPollCursor{}, err
	}
	previousFailures := failures
	failures++
	recovery := GitHubPollRecoveryConsumed
	if kind == GitHubPollFailurePreActivationInboxConflict && (previousFailures == 0 || existingKind == GitHubPollFailurePreActivationInboxConflict && existingRecovery == GitHubPollRecoveryAvailable) {
		recovery = GitHubPollRecoveryAvailable
	} else {
		kind = GitHubPollFailureUnrecoverable
	}
	delay := time.Second << min(failures-1, 6)
	_, err = tx.ExecContext(ctx, `INSERT INTO github_poll_cursors(repository, consecutive_failures, failure_kind, recovery_state, next_attempt_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(repository) DO UPDATE SET consecutive_failures = excluded.consecutive_failures, failure_kind = excluded.failure_kind, recovery_state = excluded.recovery_state, next_attempt_at = excluded.next_attempt_at, updated_at = excluded.updated_at`, repository, failures, kind, recovery, formatTimestamp(now.Add(delay)), formatTimestamp(now))
	if err != nil {
		return GitHubPollCursor{}, err
	}
	cursor, err := scanGitHubPollCursor(tx.QueryRowContext(ctx, `SELECT repository, last_success_at, last_full_reconcile_at, consecutive_failures, failure_kind, recovery_state, next_attempt_at
FROM github_poll_cursors WHERE repository = ?`, repository))
	if err != nil {
		return GitHubPollCursor{}, err
	}
	if err := tx.Commit(); err != nil {
		return GitHubPollCursor{}, err
	}
	return cursor, nil
}

func min(left, right int) int {
	if left < right {
		return left
	}
	return right
}
