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

type WorkflowQuestion struct {
	ID         string
	Repository string
	VersionID  string
	IssueID    int64
	Kind       string
	Prompt     string
	State      string
	Answer     string
	RootNumber int64
}

type GitHubPollCursor struct {
	Repository          string
	LastSuccessAt       time.Time
	LastFullReconcileAt time.Time
	ConsecutiveFailures int
	NextAttemptAt       time.Time
}

func (s *Store) GitHubPollCursor(ctx context.Context, repository string) (GitHubPollCursor, error) {
	var cursor GitHubPollCursor
	var success, full, next string
	err := s.db.QueryRowContext(ctx, `SELECT repository, last_success_at, last_full_reconcile_at, consecutive_failures, next_attempt_at
FROM github_poll_cursors WHERE repository = ?`, repository).Scan(&cursor.Repository, &success, &full, &cursor.ConsecutiveFailures, &next)
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
	_, err := s.db.ExecContext(ctx, `INSERT INTO github_poll_cursors(repository, last_success_at, last_full_reconcile_at, consecutive_failures, next_attempt_at, updated_at)
VALUES (?, ?, ?, 0, ?, ?)
ON CONFLICT(repository) DO UPDATE SET last_success_at = excluded.last_success_at,
last_full_reconcile_at = CASE WHEN excluded.last_full_reconcile_at = '' THEN github_poll_cursors.last_full_reconcile_at ELSE excluded.last_full_reconcile_at END,
consecutive_failures = 0, next_attempt_at = excluded.next_attempt_at, updated_at = excluded.updated_at`, repository, formatTimestamp(now), full, formatTimestamp(now), formatTimestamp(now))
	return err
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
	if _, err := tx.ExecContext(ctx, `UPDATE ticket_runtime SET state = ?, updated_at = ?
WHERE delivered = 0 AND version_id IN (
  SELECT current_version_id FROM plans WHERE repository = ? AND current_version_id IS NOT NULL
)`, plan.StateNeedsAttention, formatTimestamp(now), repository); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT current_version_id FROM plans WHERE repository = ? AND current_version_id IS NOT NULL`, repository)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var versionID string
		if err := rows.Scan(&versionID); err != nil {
			return err
		}
		if err := ensureWorkflowQuestionTx(ctx, tx, repository, versionID, 0, "poll_failure", "GitHub polling exhausted its retry budget. Reply with an id-addressed retry decision after resolving the GitHub access failure.", now); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) WorkflowQuestion(ctx context.Context, repository, questionID string) (WorkflowQuestion, error) {
	var question WorkflowQuestion
	err := s.db.QueryRowContext(ctx, `SELECT q.question_id, q.repository, q.version_id, q.issue_id, q.kind, q.prompt, q.state, q.answer, p.root_issue_number
FROM workflow_questions q
JOIN plan_versions v ON v.version_id = q.version_id
JOIN plans p ON p.id = v.plan_id
WHERE q.question_id = ? AND q.repository = ?`, questionID, repository).
		Scan(&question.ID, &question.Repository, &question.VersionID, &question.IssueID, &question.Kind, &question.Prompt, &question.State, &question.Answer, &question.RootNumber)
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
	var kind string
	err = tx.QueryRowContext(ctx, `SELECT kind FROM workflow_questions WHERE question_id = ? AND repository = ? AND state = 'open'`, questionID, repository).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workflow_questions SET state = 'answered', answer = ?, answered_at = ? WHERE question_id = ?`, answer, formatTimestamp(now), questionID); err != nil {
		return err
	}
	if kind == "poll_failure" {
		if _, err := tx.ExecContext(ctx, `UPDATE github_poll_cursors SET consecutive_failures = 0, next_attempt_at = ?, updated_at = ? WHERE repository = ?`, formatTimestamp(now), formatTimestamp(now), repository); err != nil {
			return err
		}
	} else if kind == "needs_attention" {
		if _, err := tx.ExecContext(ctx, `UPDATE ticket_runtime SET state = ?, updated_at = ? WHERE version_id = (SELECT version_id FROM workflow_questions WHERE question_id = ?) AND issue_id = (SELECT issue_id FROM workflow_questions WHERE question_id = ?) AND delivered = 0`, plan.StateQueued, formatTimestamp(now), questionID, questionID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func ensureWorkflowQuestionTx(ctx context.Context, tx *sql.Tx, repository, versionID string, issueID int64, kind, prompt string, now time.Time) error {
	questionID := fmt.Sprintf("%s-%s-%d", strings.ReplaceAll(kind, "_", "-"), versionID, issueID)
	_, err := tx.ExecContext(ctx, `INSERT INTO workflow_questions(question_id, repository, version_id, issue_id, kind, prompt, state, created_at)
VALUES (?, ?, ?, ?, ?, ?, 'open', ?)
ON CONFLICT(repository, version_id, issue_id, kind) DO NOTHING`, questionID, repository, versionID, issueID, kind, prompt, formatTimestamp(now))
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
	var failures int
	err = tx.QueryRowContext(ctx, `SELECT consecutive_failures FROM github_poll_cursors WHERE repository = ?`, repository).Scan(&failures)
	if errors.Is(err, sql.ErrNoRows) {
		failures = 0
	} else if err != nil {
		return err
	}
	failures++
	delay := time.Second << min(failures-1, 6)
	_, err = tx.ExecContext(ctx, `INSERT INTO github_poll_cursors(repository, consecutive_failures, next_attempt_at, updated_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(repository) DO UPDATE SET consecutive_failures = excluded.consecutive_failures, next_attempt_at = excluded.next_attempt_at, updated_at = excluded.updated_at`, repository, failures, formatTimestamp(now.Add(delay)), formatTimestamp(now))
	if err != nil {
		return err
	}
	return tx.Commit()
}

func min(left, right int) int {
	if left < right {
		return left
	}
	return right
}
