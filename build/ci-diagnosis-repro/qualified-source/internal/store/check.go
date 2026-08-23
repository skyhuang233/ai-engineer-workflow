package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

type PullRequestCheck struct {
	CheckRunID int64
	Name       string
	Status     string
	Conclusion string
	HeadSHA    string
}

func (s *Store) RecordPullRequestChecks(ctx context.Context, versionID string, issueID int64, checks []PullRequestCheck, now time.Time) (int, error) {
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
	updated := 0
	for _, check := range checks {
		check.Name = strings.TrimSpace(check.Name)
		check.Status = strings.TrimSpace(check.Status)
		check.Conclusion = strings.TrimSpace(check.Conclusion)
		check.HeadSHA = strings.TrimSpace(check.HeadSHA)
		if check.CheckRunID <= 0 || check.Name == "" || check.Status == "" || check.HeadSHA == "" {
			return 0, ErrInvalidClaim
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO pull_request_checks(version_id, issue_id, check_run_id, name, status, conclusion, head_sha, observed_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(version_id, issue_id, check_run_id) DO UPDATE SET name = excluded.name, status = excluded.status,
conclusion = excluded.conclusion, head_sha = excluded.head_sha, observed_at = excluded.observed_at
WHERE pull_request_checks.name != excluded.name OR pull_request_checks.status != excluded.status OR
pull_request_checks.conclusion != excluded.conclusion OR pull_request_checks.head_sha != excluded.head_sha`,
			versionID, issueID, check.CheckRunID, check.Name, check.Status, check.Conclusion, check.HeadSHA, formatTimestamp(now))
		if err != nil {
			return 0, err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return 0, err
		}
		updated += int(count)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return updated, nil
}
