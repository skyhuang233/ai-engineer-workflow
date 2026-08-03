package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
		questionID := fmt.Sprintf("poll-failure-%s", versionID)
		if _, err := tx.ExecContext(ctx, `INSERT INTO workflow_questions(question_id, repository, version_id, issue_id, kind, prompt, state, created_at)
VALUES (?, ?, ?, 0, 'poll_failure', ?, 'open', ?)
ON CONFLICT(repository, version_id, issue_id, kind) DO NOTHING`, questionID, repository, versionID, "GitHub polling exhausted its retry budget. Reply with an id-addressed retry decision after resolving the GitHub access failure.", formatTimestamp(now)); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return tx.Commit()
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
	}
	return tx.Commit()
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
