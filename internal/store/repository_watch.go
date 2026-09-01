package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// RepositoryWatch is the complete durable intent required to observe one
// GitHub repository. Host paths, credentials, branch policy, and failure
// diagnostics deliberately do not belong here.
type RepositoryWatch struct {
	Repository           string
	RegisteredAt         time.Time
	IssueCursor          int64
	LastSuccessfulPollAt time.Time
}

func (w RepositoryWatch) Validate() error {
	if !repositoryIdentityPattern.MatchString(w.Repository) || w.RegisteredAt.IsZero() || w.IssueCursor < 0 {
		return errors.New("invalid Repository Watch")
	}
	return nil
}

// RecordRepositoryWatch inserts a watch once. A subsequent setup invocation
// observes the original registration and boundary rather than moving either.
func (s *Store) RecordRepositoryWatch(ctx context.Context, watch RepositoryWatch) (RepositoryWatch, bool, error) {
	if err := watch.Validate(); err != nil {
		return RepositoryWatch{}, false, err
	}
	watch.Repository = strings.TrimSpace(watch.Repository)
	watch.RegisteredAt = watch.RegisteredAt.UTC()
	result, err := s.db.ExecContext(ctx, `INSERT INTO repository_watches(repository,registered_at,issue_cursor,last_successful_poll_at)
VALUES(?,?,?, '') ON CONFLICT(repository) DO NOTHING`, watch.Repository, formatTimestamp(watch.RegisteredAt), watch.IssueCursor)
	if err != nil {
		return RepositoryWatch{}, false, err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return RepositoryWatch{}, false, err
	}
	actual, err := s.RepositoryWatch(ctx, watch.Repository)
	return actual, inserted == 1, err
}

func (s *Store) RepositoryWatch(ctx context.Context, repository string) (RepositoryWatch, error) {
	return scanRepositoryWatch(s.db.QueryRowContext(ctx, `SELECT repository,registered_at,issue_cursor,last_successful_poll_at
FROM repository_watches WHERE repository = ?`, strings.TrimSpace(repository)))
}

func (s *Store) RepositoryWatches(ctx context.Context) ([]RepositoryWatch, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT repository,registered_at,issue_cursor,last_successful_poll_at
FROM repository_watches ORDER BY repository`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var watches []RepositoryWatch
	for rows.Next() {
		watch, scanErr := scanRepositoryWatch(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		watches = append(watches, watch)
	}
	return watches, rows.Err()
}

// RecordRepositoryWatchPoll advances the cursor only after the caller has
// durably handed off every issue through that cursor. A successful empty poll
// still refreshes the readiness checkpoint.
func (s *Store) RecordRepositoryWatchPoll(ctx context.Context, repository string, issueCursor int64, successfulAt time.Time) (RepositoryWatch, error) {
	if issueCursor < 0 || successfulAt.IsZero() {
		return RepositoryWatch{}, errors.New("invalid Repository Watch poll checkpoint")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE repository_watches
SET issue_cursor = CASE WHEN issue_cursor > ? THEN issue_cursor ELSE ? END,
    last_successful_poll_at = ?
WHERE repository = ?`, issueCursor, issueCursor, formatTimestamp(successfulAt.UTC()), strings.TrimSpace(repository))
	if err != nil {
		return RepositoryWatch{}, err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return RepositoryWatch{}, err
	}
	if updated != 1 {
		return RepositoryWatch{}, ErrNotFound
	}
	return s.RepositoryWatch(ctx, repository)
}

type repositoryWatchScanner interface{ Scan(...any) error }

func scanRepositoryWatch(row repositoryWatchScanner) (RepositoryWatch, error) {
	var watch RepositoryWatch
	var registered, successful string
	err := row.Scan(&watch.Repository, &registered, &watch.IssueCursor, &successful)
	if errors.Is(err, sql.ErrNoRows) {
		return RepositoryWatch{}, ErrNotFound
	}
	if err != nil {
		return RepositoryWatch{}, err
	}
	watch.RegisteredAt, err = time.Parse(time.RFC3339Nano, registered)
	if err != nil {
		return RepositoryWatch{}, err
	}
	if successful != "" {
		watch.LastSuccessfulPollAt, err = time.Parse(time.RFC3339Nano, successful)
		if err != nil {
			return RepositoryWatch{}, err
		}
	}
	return watch, watch.Validate()
}
