package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/skyhuang233/workflow/internal/plan"
	_ "modernc.org/sqlite"
)

const (
	StateProjecting = "projecting"
	StateActive     = "active"
)

var (
	ErrVersionConflict = errors.New("plan has already been activated with a different version")
	ErrNotFound        = errors.New("plan not found")
	ErrFencingConflict = errors.New("fencing conflict: ticket is already owned")
	ErrNoReadyTickets  = errors.New("no ready tickets")
	ErrCapacity        = errors.New("run capacity is full")
	ErrNotReady        = errors.New("ticket is not ready")
	ErrInvalidClaim    = errors.New("invalid ticket claim")
)

const (
	SessionRunning = "running"
	SessionClosed  = "closed"
	RunRunning     = "running"
	LeaseActive    = "active"
)

type Store struct {
	db *sql.DB
}

type PlanVersion = plan.Version

// Open configures SQLite as the durable runtime store and runs all pending
// migrations before returning a usable Store.
func Open(ctx context.Context, dsn string) (*Store, error) {
	if dsn == ":memory:" {
		dsn = "file:workflow?mode=memory&cache=shared"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db}
	if err := store.configure(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if err := store.Migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) configure(ctx context.Context) error {
	for _, statement := range []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = FULL",
		"PRAGMA foreign_keys = ON",
	} {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("configure sqlite: %w", err)
		}
	}
	return nil
}

func (s *Store) Migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER PRIMARY KEY,
    applied_at TEXT NOT NULL
)`); err != nil {
		return err
	}
	var applied int
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&applied); err != nil {
		return err
	}
	if applied < 1 {
		statements := []string{
			`CREATE TABLE plans (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repository TEXT NOT NULL,
    root_issue_id INTEGER NOT NULL,
    root_issue_number INTEGER NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('projecting', 'active')),
    current_version_id TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE (repository, root_issue_id)
)`,
			`CREATE TABLE plan_versions (
    version_id TEXT PRIMARY KEY,
    plan_id INTEGER NOT NULL REFERENCES plans(id),
    fingerprint TEXT NOT NULL,
    source_revision TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('projecting', 'active')),
    created_at TEXT NOT NULL,
    UNIQUE (plan_id, fingerprint)
)`,
			`CREATE TABLE plan_tickets (
    version_id TEXT NOT NULL REFERENCES plan_versions(version_id),
    issue_id INTEGER NOT NULL,
    issue_number INTEGER NOT NULL,
    title TEXT NOT NULL,
    body TEXT NOT NULL,
    state TEXT NOT NULL,
    delivered INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (version_id, issue_id),
    UNIQUE (version_id, issue_number)
)`,
			`CREATE TABLE plan_dependencies (
    version_id TEXT NOT NULL REFERENCES plan_versions(version_id),
    blocked_issue_id INTEGER NOT NULL,
    blocker_issue_id INTEGER NOT NULL,
    PRIMARY KEY (version_id, blocked_issue_id, blocker_issue_id),
    FOREIGN KEY (version_id, blocked_issue_id) REFERENCES plan_tickets(version_id, issue_id),
    FOREIGN KEY (version_id, blocker_issue_id) REFERENCES plan_tickets(version_id, issue_id)
)`,
		}
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("migration 1: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, applied_at) VALUES (1, ?)", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
		applied = 1
	}
	if applied < 2 {
		statements := []string{
			`CREATE TABLE ticket_sessions (
    session_id TEXT PRIMARY KEY,
    version_id TEXT NOT NULL,
    issue_id INTEGER NOT NULL,
    owner TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('running', 'closed')),
    current_run_id TEXT,
    current_lease_generation INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE (version_id, issue_id),
    FOREIGN KEY (version_id, issue_id) REFERENCES plan_tickets(version_id, issue_id)
)`,
			`CREATE TABLE worker_runs (
    run_id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES ticket_sessions(session_id),
    attempt INTEGER NOT NULL,
    lease_generation INTEGER NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('running', 'succeeded', 'failed', 'superseded', 'cancelled')),
    started_at TEXT NOT NULL,
    finished_at TEXT,
    UNIQUE (session_id, attempt),
    UNIQUE (session_id, lease_generation)
)`,
			`CREATE TABLE run_leases (
    lease_token TEXT PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES worker_runs(run_id),
    session_id TEXT NOT NULL REFERENCES ticket_sessions(session_id),
    generation INTEGER NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('active', 'expired', 'revoked')),
    expires_at TEXT NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE (session_id, generation)
)`,
			`CREATE INDEX worker_runs_running_idx ON worker_runs(state)`,
			`CREATE INDEX run_leases_live_idx ON run_leases(state, expires_at)`,
		}
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("migration 2: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, applied_at) VALUES (2, ?)", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
		applied = 2
	}
	if applied < 3 {
		if _, err := tx.ExecContext(ctx, `CREATE TABLE ticket_runtime (
    version_id TEXT NOT NULL,
    issue_id INTEGER NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('queued', 'running', 'waiting_review', 'needs_attention', 'delivered', 'cancelled')),
    delivered INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (version_id, issue_id),
    FOREIGN KEY (version_id, issue_id) REFERENCES plan_tickets(version_id, issue_id)
)`); err != nil {
			return fmt.Errorf("migration 3: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, applied_at) VALUES (3, ?)", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) Close() error { return s.db.Close() }

// BeginActivation writes a complete immutable version in one transaction.
// The returned version remains in projecting until MarkActive is called after
// the GitHub status marker has been accepted.
func (s *Store) BeginActivation(ctx context.Context, snapshot plan.Snapshot, fingerprint, sourceRevision string) (PlanVersion, error) {
	if err := snapshot.Validate(); err != nil {
		return PlanVersion{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PlanVersion{}, err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `INSERT INTO plans(repository, root_issue_id, root_issue_number, state, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(repository, root_issue_id) DO NOTHING`, snapshot.Repository, snapshot.Root.ID, snapshot.Root.Number, StateProjecting, now, now); err != nil {
		return PlanVersion{}, err
	}
	var planID int64
	var currentID sql.NullString
	var currentState string
	if err := tx.QueryRowContext(ctx, `SELECT id, current_version_id, state FROM plans WHERE repository = ? AND root_issue_id = ?`, snapshot.Repository, snapshot.Root.ID).Scan(&planID, &currentID, &currentState); err != nil {
		return PlanVersion{}, err
	}
	if currentID.Valid {
		var version PlanVersion
		if err := tx.QueryRowContext(ctx, `SELECT version_id, fingerprint, source_revision, state FROM plan_versions WHERE version_id = ?`, currentID.String).Scan(&version.ID, &version.Fingerprint, &version.SourceRevision, &version.State); err != nil {
			return PlanVersion{}, err
		}
		if version.Fingerprint != fingerprint {
			return PlanVersion{}, ErrVersionConflict
		}
		version.Repository = snapshot.Repository
		version.RootIssueID = snapshot.Root.ID
		version.RootIssueNumber = snapshot.Root.Number
		if err := tx.Commit(); err != nil {
			return PlanVersion{}, err
		}
		return version, nil
	}

	versionID := "pv-" + fingerprint
	if _, err := tx.ExecContext(ctx, `INSERT INTO plan_versions(version_id, plan_id, fingerprint, source_revision, state, created_at)
VALUES (?, ?, ?, ?, ?, ?)`, versionID, planID, fingerprint, sourceRevision, StateProjecting, now); err != nil {
		return PlanVersion{}, err
	}
	for _, ticket := range snapshot.Tickets() {
		if _, err := tx.ExecContext(ctx, `INSERT INTO plan_tickets(version_id, issue_id, issue_number, title, body, state, delivered)
VALUES (?, ?, ?, ?, ?, ?, ?)`, versionID, ticket.ID, ticket.Number, ticket.Title, ticket.Body, ticket.State, boolInt(ticket.IsDelivered())); err != nil {
			return PlanVersion{}, err
		}
		runtimeState := plan.StateQueued
		if ticket.IsDelivered() {
			runtimeState = plan.StateDelivered
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO ticket_runtime(version_id, issue_id, state, delivered, updated_at)
VALUES (?, ?, ?, ?, ?)`, versionID, ticket.ID, runtimeState, boolInt(ticket.IsDelivered()), now); err != nil {
			return PlanVersion{}, err
		}
	}
	for blockedID, blockers := range snapshot.BlockedBy {
		for _, blocker := range blockers {
			if _, err := tx.ExecContext(ctx, `INSERT INTO plan_dependencies(version_id, blocked_issue_id, blocker_issue_id) VALUES (?, ?, ?)`, versionID, blockedID, blocker.ID); err != nil {
				return PlanVersion{}, err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE plans SET current_version_id = ?, state = ?, updated_at = ? WHERE id = ?`, versionID, StateProjecting, now, planID); err != nil {
		return PlanVersion{}, err
	}
	if err := tx.Commit(); err != nil {
		return PlanVersion{}, err
	}
	return PlanVersion{ID: versionID, Repository: snapshot.Repository, RootIssueID: snapshot.Root.ID, RootIssueNumber: snapshot.Root.Number, Fingerprint: fingerprint, SourceRevision: sourceRevision, State: StateProjecting}, nil
}

func (s *Store) MarkActive(ctx context.Context, versionID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE plan_versions SET state = ? WHERE version_id = ? AND state = ?`, StateActive, versionID, StateProjecting)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		var state string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM plan_versions WHERE version_id = ?`, versionID).Scan(&state); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if state != StateActive {
			return ErrVersionConflict
		}
	}
	result, err = tx.ExecContext(ctx, `UPDATE plans SET state = ?, updated_at = ? WHERE current_version_id = ? AND state = ?`, StateActive, time.Now().UTC().Format(time.RFC3339Nano), versionID, StateProjecting)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		var state string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM plans WHERE current_version_id = ?`, versionID).Scan(&state); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrVersionConflict
			}
			return err
		}
		if state != StateActive {
			return ErrVersionConflict
		}
	}
	return tx.Commit()
}

func (s *Store) CurrentVersion(ctx context.Context, repository string, rootIssueID int64) (PlanVersion, error) {
	var version PlanVersion
	err := s.db.QueryRowContext(ctx, `SELECT v.version_id, p.repository, p.root_issue_id, p.root_issue_number, v.fingerprint, v.source_revision, v.state
FROM plans p JOIN plan_versions v ON v.version_id = p.current_version_id WHERE p.repository = ? AND p.root_issue_id = ?`, repository, rootIssueID).
		Scan(&version.ID, &version.Repository, &version.RootIssueID, &version.RootIssueNumber, &version.Fingerprint, &version.SourceRevision, &version.State)
	if errors.Is(err, sql.ErrNoRows) {
		return PlanVersion{}, ErrNotFound
	}
	return version, err
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
