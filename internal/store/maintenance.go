package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ErrMaintenanceFenced is returned by every scheduling claim while a Setup
// Launcher owns the bounded maintenance window for this generation.
var ErrMaintenanceFenced = errors.New("platform maintenance fence is active")

type MaintenanceFence struct {
	OperationID  string
	BundleDigest string
	CreatedAt    time.Time
}

// ActiveWorkerRuns is the authoritative read-only active-work fact exposed to
// the Setup Launcher preflight. It deliberately returns identifiers as well as
// a count so the skill can explain why an upgrade must wait.
func (s *Store) ActiveWorkerRuns(ctx context.Context, now time.Time) (int, []string, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	rows, err := s.db.QueryContext(ctx, `SELECT r.run_id FROM worker_runs r
JOIN run_leases l ON l.run_id = r.run_id AND l.generation = r.lease_generation
WHERE r.state = ? AND l.state = ? AND l.expires_at > ? ORDER BY r.run_id`, RunRunning, LeaseActive, formatTimestamp(now))
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return 0, nil, err
		}
		ids = append(ids, id)
	}
	return len(ids), ids, rows.Err()
}

// BeginMaintenanceFence atomically writes the target-bound fence and counts
// active runs. A non-zero count removes the new fence before committing, so a
// failed upgrade preflight never leaves scheduling paused.
func (s *Store) BeginMaintenanceFence(ctx context.Context, fence MaintenanceFence, now time.Time) (int, error) {
	if fence.OperationID == "" || fence.BundleDigest == "" {
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
	if _, err := tx.ExecContext(ctx, `INSERT INTO platform_maintenance_fences(singleton, operation_id, bundle_digest, created_at) VALUES(1, ?, ?, ?)`, fence.OperationID, fence.BundleDigest, formatTimestamp(now)); err != nil {
		return 0, err
	}
	count, err := activeRunCountTx(ctx, tx, now)
	if err != nil {
		return 0, err
	}
	if count != 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM platform_maintenance_fences WHERE singleton=1 AND operation_id=?`, fence.OperationID); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}

func (s *Store) ClearMaintenanceFence(ctx context.Context, operationID string) error {
	if operationID == "" {
		return ErrInvalidClaim
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM platform_maintenance_fences WHERE singleton=1 AND operation_id=?`, operationID)
	return err
}

// ClearMaintenanceFenceForBundle completes forward repair of an already
// activated generation.  Its active.json and retained Attempt bind this call
// to the same immutable target; there is no prior generation to restore or
// a new maintenance operation to guess.
func (s *Store) ClearMaintenanceFenceForBundle(ctx context.Context, bundleDigest string) error {
	if bundleDigest == "" {
		return ErrInvalidClaim
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM platform_maintenance_fences WHERE singleton=1 AND bundle_digest=?`, bundleDigest)
	return err
}

func maintenanceFencedTx(ctx context.Context, tx *sql.Tx) (bool, error) {
	var present int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM platform_maintenance_fences WHERE singleton=1`).Scan(&present)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
