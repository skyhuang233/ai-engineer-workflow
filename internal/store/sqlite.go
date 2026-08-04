package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/skyhuang233/workflow/internal/plan"
	_ "modernc.org/sqlite"
)

const (
	StateProjecting     = "projecting"
	StateActive         = "active"
	StateCompleted      = "completed"
	latestSchemaVersion = 28
)

var (
	ErrVersionConflict     = errors.New("plan has already been activated with a different version")
	ErrNotFound            = errors.New("plan not found")
	ErrFencingConflict     = errors.New("fencing conflict: ticket is already owned")
	ErrNoReadyTickets      = errors.New("no ready tickets")
	ErrCapacity            = errors.New("run capacity is full")
	ErrNotReady            = errors.New("ticket is not ready")
	ErrInvalidClaim        = errors.New("invalid ticket claim")
	ErrDeliveryInProgress  = errors.New("delivery outbox item is already being processed")
	ErrGatewayWritesPaused = errors.New("gateway writes are paused")
	ErrWorkerLaunched      = errors.New("worker run has already been launched")
	ErrNeedsAttention      = errors.New("workflow needs attention")
)

const (
	SessionRunning = "running"
	SessionClosed  = "closed"
	RunRunning     = "running"
	LeaseActive    = "active"
	RunAgent       = "agent"
	RunDelivery    = "delivery_controller"
)

type Store struct {
	db           *sql.DB
	databasePath string
	leaseMu      sync.Mutex
}

type PlanVersion = plan.Version

const timestampLayout = "2006-01-02T15:04:05.000000000Z"

func formatTimestamp(value time.Time) string {
	return value.UTC().Format(timestampLayout)
}

// Open configures SQLite as the durable runtime store and runs all pending
// migrations before returning a usable Store.
func Open(ctx context.Context, dsn string) (*Store, error) {
	databasePath := ""
	if dsn != ":memory:" && !strings.HasPrefix(dsn, "file:") {
		databasePath = dsn
	}
	if dsn == ":memory:" {
		dsn = "file:workflow?mode=memory&cache=shared"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db, databasePath: databasePath}
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
	applied, err := s.schemaVersion(ctx)
	if err != nil {
		return err
	}
	if applied < latestSchemaVersion {
		if err := s.backupDatabase(ctx); err != nil {
			return err
		}
	}
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
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, applied_at) VALUES (1, ?)", formatTimestamp(time.Now())); err != nil {
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
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, applied_at) VALUES (2, ?)", formatTimestamp(time.Now())); err != nil {
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
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, applied_at) VALUES (3, ?)", formatTimestamp(time.Now())); err != nil {
			return err
		}
	}
	if applied < 4 {
		statements := []string{
			`ALTER TABLE ticket_sessions ADD COLUMN agent_identity TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE ticket_sessions ADD COLUMN codex_session_id TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE ticket_sessions ADD COLUMN workspace_path TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE ticket_sessions ADD COLUMN codex_state_path TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE ticket_sessions ADD COLUMN branch TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE ticket_sessions ADD COLUMN accepted_commit TEXT NOT NULL DEFAULT ''`,
			`CREATE TABLE worker_audits (
    run_id TEXT PRIMARY KEY REFERENCES worker_runs(run_id),
    container_id TEXT NOT NULL,
    image_digest TEXT NOT NULL,
    mounts_json TEXT NOT NULL,
    tool_versions_json TEXT NOT NULL,
    github_write_credentials INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL
)`,
			`CREATE TABLE candidate_revisions (
    run_id TEXT PRIMARY KEY REFERENCES worker_runs(run_id),
    session_id TEXT NOT NULL REFERENCES ticket_sessions(session_id),
    codex_session_id TEXT NOT NULL,
    commit_sha TEXT NOT NULL,
    structured_output TEXT NOT NULL,
    created_at TEXT NOT NULL
)`,
			`CREATE TABLE run_diagnostics (
    run_id TEXT PRIMARY KEY REFERENCES worker_runs(run_id),
    diagnostics_path TEXT NOT NULL,
    error TEXT NOT NULL,
    created_at TEXT NOT NULL
)`,
		}
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("migration 4: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, applied_at) VALUES (4, ?)", formatTimestamp(time.Now())); err != nil {
			return err
		}
	}
	if applied < 5 {
		statements := []string{
			`CREATE TABLE ticket_deliveries (
    version_id TEXT NOT NULL,
    issue_id INTEGER NOT NULL,
    repository TEXT NOT NULL,
    branch TEXT NOT NULL,
    pull_request_number INTEGER NOT NULL DEFAULT 0,
    pull_request_node_id TEXT NOT NULL DEFAULT '',
    remote_head TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (version_id, issue_id),
    FOREIGN KEY (version_id, issue_id) REFERENCES plan_tickets(version_id, issue_id)
)`,
			`CREATE TABLE delivery_outbox (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    idempotency_key TEXT NOT NULL UNIQUE,
    operation TEXT NOT NULL,
    request_json TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('pending', 'processing', 'succeeded', 'rejected')),
    attempts INTEGER NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    completed_at TEXT
)`,
			`CREATE TABLE delivery_audits (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    idempotency_key TEXT NOT NULL DEFAULT '',
    operation TEXT NOT NULL,
    run_id TEXT NOT NULL DEFAULT '',
    lease_generation INTEGER NOT NULL DEFAULT 0,
    decision TEXT NOT NULL,
    reason TEXT NOT NULL,
    created_at TEXT NOT NULL
)`,
			`CREATE INDEX delivery_outbox_state_idx ON delivery_outbox(state, updated_at)`,
		}
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("migration 5: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, applied_at) VALUES (5, ?)", formatTimestamp(time.Now())); err != nil {
			return err
		}
	}
	if applied < 6 {
		statements := []string{
			`ALTER TABLE delivery_outbox ADD COLUMN claim_token TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE delivery_outbox ADD COLUMN next_attempt_at TEXT NOT NULL DEFAULT ''`,
			`UPDATE run_leases SET expires_at = strftime('%Y-%m-%dT%H:%M:%f000000Z', expires_at)`,
			`UPDATE delivery_outbox SET updated_at = strftime('%Y-%m-%dT%H:%M:%f000000Z', updated_at), next_attempt_at = strftime('%Y-%m-%dT%H:%M:%f000000Z', updated_at)`,
		}
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("migration 6: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, applied_at) VALUES (6, ?)", formatTimestamp(time.Now())); err != nil {
			return err
		}
	}
	if applied < 7 {
		statements := []string{
			`ALTER TABLE worker_audits ADD COLUMN extra_hosts_json TEXT NOT NULL DEFAULT '[]'`,
			`ALTER TABLE delivery_outbox ADD COLUMN uncertain INTEGER NOT NULL DEFAULT 0`,
		}
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("migration 7: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, applied_at) VALUES (7, ?)", formatTimestamp(time.Now())); err != nil {
			return err
		}
	}
	if applied < 8 {
		statements := []string{
			`CREATE TABLE review_feedback_events (
    version_id TEXT NOT NULL,
    issue_id INTEGER NOT NULL,
    source TEXT NOT NULL,
    event_id TEXT NOT NULL,
    author TEXT NOT NULL,
    body TEXT NOT NULL,
    received_at TEXT NOT NULL,
    claimed_run_id TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (version_id, issue_id, source, event_id),
    FOREIGN KEY (version_id, issue_id) REFERENCES plan_tickets(version_id, issue_id)
)`,
			`CREATE INDEX review_feedback_unclaimed_idx ON review_feedback_events(version_id, issue_id, claimed_run_id, received_at)`,
			`CREATE TABLE plan_freezes (
    version_id TEXT PRIMARY KEY REFERENCES plan_versions(version_id),
    issue_id INTEGER NOT NULL,
    reason TEXT NOT NULL,
    frozen_at TEXT NOT NULL,
    FOREIGN KEY (version_id, issue_id) REFERENCES plan_tickets(version_id, issue_id)
)`,
		}
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("migration 8: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, applied_at) VALUES (8, ?)", formatTimestamp(time.Now())); err != nil {
			return err
		}
	}
	if applied < 9 {
		statements := []string{
			`ALTER TABLE worker_runs ADD COLUMN launch_state TEXT NOT NULL DEFAULT 'ready' CHECK (launch_state IN ('ready', 'launched'))`,
			`ALTER TABLE worker_runs ADD COLUMN launched_at TEXT`,
		}
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("migration 9: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, applied_at) VALUES (9, ?)", formatTimestamp(time.Now())); err != nil {
			return err
		}
	}
	if applied < 10 {
		if _, err := tx.ExecContext(ctx, `CREATE TABLE github_poll_cursors (
    repository TEXT PRIMARY KEY,
    last_success_at TEXT NOT NULL DEFAULT '',
    last_full_reconcile_at TEXT NOT NULL DEFAULT '',
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL
)`); err != nil {
			return fmt.Errorf("migration 10: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, applied_at) VALUES (10, ?)", formatTimestamp(time.Now())); err != nil {
			return err
		}
	}
	if applied < 11 {
		if _, err := tx.ExecContext(ctx, `CREATE TABLE pull_request_checks (
    version_id TEXT NOT NULL,
    issue_id INTEGER NOT NULL,
    check_run_id INTEGER NOT NULL,
    name TEXT NOT NULL,
    status TEXT NOT NULL,
    conclusion TEXT NOT NULL DEFAULT '',
    head_sha TEXT NOT NULL,
    observed_at TEXT NOT NULL,
    PRIMARY KEY (version_id, issue_id, check_run_id),
    FOREIGN KEY (version_id, issue_id) REFERENCES plan_tickets(version_id, issue_id)
)`); err != nil {
			return fmt.Errorf("migration 11: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, applied_at) VALUES (11, ?)", formatTimestamp(time.Now())); err != nil {
			return err
		}
	}
	if applied < 12 {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE ticket_sessions ADD COLUMN consecutive_failures INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("migration 12: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, applied_at) VALUES (12, ?)", formatTimestamp(time.Now())); err != nil {
			return err
		}
	}
	if applied < 13 {
		statements := []string{
			`ALTER TABLE ticket_deliveries ADD COLUMN checks_etag TEXT NOT NULL DEFAULT ''`,
			`CREATE TABLE workflow_questions (
    question_id TEXT PRIMARY KEY,
    repository TEXT NOT NULL,
    version_id TEXT NOT NULL,
    issue_id INTEGER NOT NULL DEFAULT 0,
    kind TEXT NOT NULL,
    prompt TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('open', 'answered')),
    answer TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    answered_at TEXT NOT NULL DEFAULT '',
    UNIQUE(repository, version_id, issue_id, kind),
    FOREIGN KEY (version_id) REFERENCES plan_versions(version_id)
)`,
		}
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("migration 13: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, applied_at) VALUES (13, ?)", formatTimestamp(time.Now())); err != nil {
			return err
		}
	}
	if applied < 14 {
		statements := []string{
			`ALTER TABLE ticket_sessions ADD COLUMN recovery_epoch INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE worker_runs ADD COLUMN recovery_epoch INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE workflow_questions RENAME TO workflow_questions_old`,
			`CREATE TABLE workflow_questions (
    question_id TEXT PRIMARY KEY,
    repository TEXT NOT NULL,
    version_id TEXT NOT NULL,
    issue_id INTEGER NOT NULL DEFAULT 0,
    kind TEXT NOT NULL,
    generation INTEGER NOT NULL,
    prompt TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('open', 'answered')),
    answer TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    answered_at TEXT NOT NULL DEFAULT '',
    UNIQUE(repository, version_id, issue_id, kind, generation),
    FOREIGN KEY (version_id) REFERENCES plan_versions(version_id)
)`,
			`INSERT INTO workflow_questions(question_id, repository, version_id, issue_id, kind, generation, prompt, state, answer, created_at, answered_at)
SELECT question_id, repository, version_id, issue_id, kind, 1, prompt, state, answer, created_at, answered_at FROM workflow_questions_old`,
			`DROP TABLE workflow_questions_old`,
		}
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("migration 14: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, applied_at) VALUES (14, ?)", formatTimestamp(time.Now())); err != nil {
			return err
		}
	}
	if applied < 15 {
		statements := []string{
			`CREATE TABLE poll_failure_targets (
    poll_question_id TEXT NOT NULL REFERENCES workflow_questions(question_id),
    version_id TEXT NOT NULL,
    issue_id INTEGER NOT NULL,
    ticket_question_id TEXT NOT NULL REFERENCES workflow_questions(question_id),
    PRIMARY KEY (poll_question_id, version_id, issue_id)
)`,
		}
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("migration 15: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, applied_at) VALUES (15, ?)", formatTimestamp(time.Now())); err != nil {
			return err
		}
	}
	if applied < 16 {
		if _, err := tx.ExecContext(ctx, `CREATE TABLE accepted_candidate_outbox (
    outbox_key TEXT PRIMARY KEY REFERENCES delivery_outbox(idempotency_key),
    run_id TEXT NOT NULL REFERENCES worker_runs(run_id),
    created_at TEXT NOT NULL
)`); err != nil {
			return fmt.Errorf("migration 16: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, applied_at) VALUES (16, ?)", formatTimestamp(time.Now())); err != nil {
			return err
		}
	}
	if applied < 17 {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE worker_runs ADD COLUMN run_kind TEXT NOT NULL DEFAULT 'agent' CHECK (run_kind IN ('agent', 'delivery_controller'))`); err != nil {
			return fmt.Errorf("migration 17: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, applied_at) VALUES (17, ?)", formatTimestamp(time.Now())); err != nil {
			return err
		}
	}
	if applied < 18 {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE ticket_sessions ADD COLUMN delivery_retry_pending INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("migration 18: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, applied_at) VALUES (18, ?)", formatTimestamp(time.Now())); err != nil {
			return err
		}
	}
	if applied < 19 {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE ticket_sessions ADD COLUMN workspace_reclaimed_at TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("migration 19: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, applied_at) VALUES (19, ?)", formatTimestamp(time.Now())); err != nil {
			return err
		}
	}
	if applied < 20 {
		if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS completed_plan_versions (
    version_id TEXT PRIMARY KEY REFERENCES plan_versions(version_id),
    completed_at TEXT NOT NULL
)`); err != nil {
			return fmt.Errorf("migration 20: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, applied_at) VALUES (20, ?)", formatTimestamp(time.Now())); err != nil {
			return err
		}
	}
	if applied < 21 {
		if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS plan_terminal_states (
    version_id TEXT PRIMARY KEY REFERENCES plan_versions(version_id),
    state TEXT NOT NULL CHECK (state IN ('completed', 'cancelled')),
    recorded_at TEXT NOT NULL
)`); err != nil {
			return fmt.Errorf("migration 21: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, applied_at) VALUES (21, ?)", formatTimestamp(time.Now())); err != nil {
			return err
		}
	}
	if applied < 22 {
		statements := []string{
			`CREATE TABLE IF NOT EXISTS workflow_question_contexts (
    question_id TEXT PRIMARY KEY REFERENCES workflow_questions(question_id),
    ticket_number INTEGER NOT NULL,
    pull_request_number INTEGER NOT NULL DEFAULT 0,
    accepted_commit TEXT NOT NULL DEFAULT '',
    diagnostics_path TEXT NOT NULL DEFAULT '',
    candidate_evidence TEXT NOT NULL DEFAULT ''
)`,
			`CREATE TABLE IF NOT EXISTS replacement_tickets (
    question_id TEXT PRIMARY KEY REFERENCES workflow_questions(question_id),
    version_id TEXT NOT NULL REFERENCES plan_versions(version_id),
    retired_issue_id INTEGER NOT NULL,
    replacement TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('active')),
    approved_at TEXT NOT NULL,
    FOREIGN KEY (version_id, retired_issue_id) REFERENCES plan_tickets(version_id, issue_id)
)`,
		}
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("migration 22: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, applied_at) VALUES (22, ?)", formatTimestamp(time.Now())); err != nil {
			return err
		}
		applied = 22
	}
	if applied < 23 {
		columns := []struct {
			name       string
			definition string
		}{
			{name: "replacement_version_id", definition: "TEXT NOT NULL DEFAULT ''"},
			{name: "replacement_issue_id", definition: "INTEGER NOT NULL DEFAULT 0"},
		}
		for _, column := range columns {
			exists, err := tableHasColumnTx(ctx, tx, "replacement_tickets", column.name)
			if err != nil {
				return fmt.Errorf("migration 23: %w", err)
			}
			if !exists {
				if _, err := tx.ExecContext(ctx, "ALTER TABLE replacement_tickets ADD COLUMN "+column.name+" "+column.definition); err != nil {
					return fmt.Errorf("migration 23: %w", err)
				}
			}
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, applied_at) VALUES (23, ?)", formatTimestamp(time.Now())); err != nil {
			return err
		}
		applied = 23
	}
	if applied < 24 {
		statements := []string{
			`CREATE TABLE replacement_tickets_v24 (
    question_id TEXT PRIMARY KEY REFERENCES workflow_questions(question_id),
    version_id TEXT NOT NULL REFERENCES plan_versions(version_id),
    retired_issue_id INTEGER NOT NULL,
    replacement TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('approved', 'active')),
    approved_at TEXT NOT NULL,
    replacement_version_id TEXT NOT NULL DEFAULT '',
    replacement_issue_id INTEGER NOT NULL DEFAULT 0,
    FOREIGN KEY (version_id, retired_issue_id) REFERENCES plan_tickets(version_id, issue_id)
)`,
			`INSERT INTO replacement_tickets_v24(question_id, version_id, retired_issue_id, replacement, state, approved_at, replacement_version_id, replacement_issue_id)
SELECT question_id, version_id, retired_issue_id, replacement, state, approved_at, replacement_version_id, replacement_issue_id FROM replacement_tickets`,
			`DROP TABLE replacement_tickets`,
			`ALTER TABLE replacement_tickets_v24 RENAME TO replacement_tickets`,
		}
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("migration 24: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, applied_at) VALUES (24, ?)", formatTimestamp(time.Now())); err != nil {
			return err
		}
		applied = 24
	}
	if applied < 25 {
		statements := []string{
			`CREATE TABLE IF NOT EXISTS worker_releases (
    image_digest TEXT PRIMARY KEY,
    version TEXT NOT NULL,
    source_commit TEXT NOT NULL,
    manifest_json TEXT NOT NULL,
    verified_at TEXT NOT NULL,
    activated_at TEXT NOT NULL
)`,
			`CREATE TABLE IF NOT EXISTS active_worker_image (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    image_digest TEXT NOT NULL REFERENCES worker_releases(image_digest)
)`,
		}
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("migration 25: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, applied_at) VALUES (25, ?)", formatTimestamp(time.Now())); err != nil {
			return err
		}
		applied = 25
	}
	if applied < 26 {
		if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS gateway_credential_verifications (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    fingerprint_sha256 TEXT NOT NULL,
    owner TEXT NOT NULL,
    integration_repository TEXT NOT NULL,
    verified_at TEXT NOT NULL
)`); err != nil {
			return fmt.Errorf("migration 26: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, applied_at) VALUES (26, ?)", formatTimestamp(time.Now())); err != nil {
			return err
		}
		applied = 26
	}
	if applied < 27 {
		statements := []string{
			`CREATE TABLE IF NOT EXISTS gateway_runtime (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    writes_paused INTEGER NOT NULL DEFAULT 0,
    reason TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL
)`,
			`CREATE TABLE IF NOT EXISTS workflow_inbox (
    item_key TEXT PRIMARY KEY,
    kind TEXT NOT NULL,
    title TEXT NOT NULL,
    body TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('open', 'resolved')),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
)`,
		}
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("migration 27: %w", err)
			}
		}
		timestamp := formatTimestamp(time.Now())
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO gateway_runtime(singleton, writes_paused, reason, updated_at) VALUES (1, 0, '', ?)`, timestamp); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, applied_at) VALUES (27, ?)", timestamp); err != nil {
			return err
		}
		applied = 27
	}
	if applied < 28 {
		columns := []struct {
			table      string
			name       string
			definition string
		}{
			{table: "candidate_revisions", name: "image_digest", definition: "TEXT NOT NULL DEFAULT ''"},
			{table: "candidate_revisions", name: "tool_versions_json", definition: "TEXT NOT NULL DEFAULT '{}'"},
			{table: "ticket_sessions", name: "accepted_candidate_run_id", definition: "TEXT NOT NULL DEFAULT ''"},
		}
		for _, column := range columns {
			exists, err := tableHasColumnTx(ctx, tx, column.table, column.name)
			if err != nil {
				return fmt.Errorf("migration 28: %w", err)
			}
			if !exists {
				if _, err := tx.ExecContext(ctx, "ALTER TABLE "+column.table+" ADD COLUMN "+column.name+" "+column.definition); err != nil {
					return fmt.Errorf("migration 28: %w", err)
				}
			}
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, applied_at) VALUES (28, ?)", formatTimestamp(time.Now())); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func tableHasColumnTx(ctx context.Context, tx *sql.Tx, table, column string) (bool, error) {
	rows, err := tx.QueryContext(ctx, "SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (s *Store) schemaVersion(ctx context.Context) (int, error) {
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations'`).Scan(&exists); err != nil {
		return 0, err
	}
	if exists == 0 {
		return 0, nil
	}
	var applied int
	if err := s.db.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&applied); err != nil {
		return 0, err
	}
	return applied, nil
}

func (s *Store) backupDatabase(ctx context.Context) error {
	if s.databasePath == "" {
		return nil
	}
	if _, err := os.Stat(s.databasePath); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return fmt.Errorf("checkpoint sqlite before migration backup: %w", err)
	}
	data, err := os.ReadFile(s.databasePath)
	if err != nil {
		return fmt.Errorf("read sqlite migration backup: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(s.databasePath), filepath.Base(s.databasePath)+".migration-*.tmp")
	if err != nil {
		return fmt.Errorf("create sqlite migration backup: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("secure sqlite migration backup: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("write sqlite migration backup: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync sqlite migration backup: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close sqlite migration backup: %w", err)
	}
	backup, err := sql.Open("sqlite", temporaryPath)
	if err != nil {
		return fmt.Errorf("open sqlite migration backup: %w", err)
	}
	var result string
	if err := backup.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&result); err != nil {
		backup.Close()
		return fmt.Errorf("verify sqlite migration backup: %w", err)
	}
	if result != "ok" {
		backup.Close()
		return fmt.Errorf("verify sqlite migration backup: integrity check = %q", result)
	}
	if err := backup.Close(); err != nil {
		return fmt.Errorf("close verified sqlite migration backup: %w", err)
	}
	backupPath := s.databasePath + ".migration.bak"
	archivedPath := ""
	if _, err := os.Stat(backupPath); err == nil {
		archivedPath = fmt.Sprintf("%s.migration.%d.bak", s.databasePath, time.Now().UTC().UnixNano())
		if err := os.Rename(backupPath, archivedPath); err != nil {
			return fmt.Errorf("preserve previous sqlite migration backup: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(temporaryPath, backupPath); err != nil {
		if archivedPath != "" {
			_ = os.Rename(archivedPath, backupPath)
		}
		return fmt.Errorf("publish sqlite migration backup: %w", err)
	}
	return nil
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
	now := formatTimestamp(time.Now())
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
		if err := tx.QueryRowContext(ctx, `SELECT v.version_id, v.fingerprint, v.source_revision,
COALESCE((SELECT state FROM plan_terminal_states WHERE version_id = v.version_id), CASE WHEN EXISTS (SELECT 1 FROM completed_plan_versions WHERE version_id = v.version_id) THEN ? ELSE v.state END)
FROM plan_versions v WHERE v.version_id = ?`, StateCompleted, currentID.String).Scan(&version.ID, &version.Fingerprint, &version.SourceRevision, &version.State); err != nil {
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
	result, err = tx.ExecContext(ctx, `UPDATE plans SET state = ?, updated_at = ? WHERE current_version_id = ? AND state = ?`, StateActive, formatTimestamp(time.Now()), versionID, StateProjecting)
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
	if err := markPlanCompletedTx(ctx, tx, versionID, formatTimestamp(time.Now())); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ActivationState(ctx context.Context, versionID string) (string, error) {
	var state string
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE((SELECT terminal.state FROM plan_terminal_states terminal WHERE terminal.version_id = v.version_id), CASE WHEN EXISTS (SELECT 1 FROM completed_plan_versions completed WHERE completed.version_id = v.version_id) THEN ? ELSE v.state END)
FROM plan_versions v WHERE v.version_id = ?`, StateCompleted, versionID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return state, err
}

func (s *Store) SnapshotWithDeliveryFacts(ctx context.Context, snapshot plan.Snapshot) (plan.Snapshot, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT r.issue_id
FROM plans p
JOIN plan_versions v ON v.version_id = p.current_version_id
JOIN ticket_runtime r ON r.version_id = v.version_id
WHERE p.repository = ? AND p.root_issue_id = ? AND r.delivered = 1`, snapshot.Repository, snapshot.Root.ID)
	if err != nil {
		return plan.Snapshot{}, err
	}
	defer rows.Close()
	delivered := make(map[int64]struct{})
	for rows.Next() {
		var issueID int64
		if err := rows.Scan(&issueID); err != nil {
			return plan.Snapshot{}, err
		}
		delivered[issueID] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return plan.Snapshot{}, err
	}
	for index := range snapshot.Children {
		if _, ok := delivered[snapshot.Children[index].ID]; ok {
			snapshot.Children[index].Delivered = true
		}
	}
	for blockedID, blockers := range snapshot.BlockedBy {
		for index := range blockers {
			if _, ok := delivered[blockers[index].ID]; ok {
				blockers[index].Delivered = true
			}
		}
		snapshot.BlockedBy[blockedID] = blockers
	}
	return snapshot, nil
}

func (s *Store) CurrentVersion(ctx context.Context, repository string, rootIssueID int64) (PlanVersion, error) {
	var version PlanVersion
	err := s.db.QueryRowContext(ctx, `SELECT v.version_id, p.repository, p.root_issue_id, p.root_issue_number, v.fingerprint, v.source_revision,
COALESCE((SELECT state FROM plan_terminal_states WHERE version_id = v.version_id), CASE WHEN EXISTS (SELECT 1 FROM completed_plan_versions WHERE version_id = v.version_id) THEN ? ELSE v.state END)
FROM plans p JOIN plan_versions v ON v.version_id = p.current_version_id WHERE p.repository = ? AND p.root_issue_id = ?`, StateCompleted, repository, rootIssueID).
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
