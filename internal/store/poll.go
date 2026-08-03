package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/skyhuang233/workflow/internal/plan"
)

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
	_, err := s.db.ExecContext(ctx, `UPDATE ticket_runtime SET state = ?, updated_at = ?
WHERE delivered = 0 AND version_id IN (
  SELECT current_version_id FROM plans WHERE repository = ? AND current_version_id IS NOT NULL
)`, plan.StateNeedsAttention, formatTimestamp(now), repository)
	return err
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
