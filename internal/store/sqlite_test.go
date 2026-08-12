package store

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skyhuang233/workflow/internal/plan"
	"github.com/skyhuang233/workflow/internal/startup"
)

func TestOpenFileURIHoldsCanonicalRestoreBarrier(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workflow.db")
	fileURI := (&url.URL{Scheme: "file", Path: "/" + strings.TrimLeft(filepath.ToSlash(path), "/")}).String()
	db, err := Open(context.Background(), fileURI)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	blocked, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := startup.AcquireRestoreBarrier(blocked, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("restore bypassed Store opened by file URI: %v", err)
	}
}

func TestOpenForRuntimeSkipsMigrationDiscoveryAndHoldsRestoreBarrier(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "workflow.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version = 55`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `DROP TABLE delivery_write_fences`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	runtimeStore, err := OpenForRuntime(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer runtimeStore.Close()
	var tables int
	if err := runtimeStore.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'delivery_write_fences'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Fatal("runtime open unexpectedly ran pending migration")
	}
	blocked, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := startup.AcquireRestoreBarrier(blocked, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("restore crossed runtime Store database access: %v", err)
	}
}

func TestSQLiteMigrationActivationAndRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "workflow.db")
	ctx := context.Background()
	snapshot := testSnapshot()
	fingerprint, err := snapshot.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}

	store, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.BeginActivation(ctx, snapshot, fingerprint, "root-revision-1")
	if err != nil {
		t.Fatal(err)
	}
	if first.State != StateProjecting {
		t.Fatalf("initial state = %q, want projecting", first.State)
	}
	if err := store.MarkActive(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	second, err := store.BeginActivation(ctx, snapshot, fingerprint, "root-revision-2")
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || second.State != StateActive {
		t.Fatalf("duplicate activation = %#v, want same active version", second)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	recovered, err := restarted.CurrentVersion(ctx, snapshot.Repository, snapshot.Root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.ID != first.ID || recovered.State != StateActive {
		t.Fatalf("recovered = %#v, want persisted active version", recovered)
	}
}

func TestCurrentSchemaVersionMatchesLatestMigration(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	version, err := store.schemaVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if version != latestSchemaVersion {
		t.Fatalf("schema version = %d, want %d", version, latestSchemaVersion)
	}
}

func TestMigrationFromV50AddsDeliverySourceDigest(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "workflow.db")
	store, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "DELETE FROM schema_migrations WHERE version >= 51"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "ALTER TABLE candidate_revisions DROP COLUMN delivery_source_digest"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	migrated, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	if !hasColumn(t, ctx, migrated.db, "candidate_revisions", "delivery_source_digest") {
		t.Fatal("migration did not add the Delivery Source digest")
	}
}

func TestMigrationFromV39AddsQualityGateQuestions(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "workflow.db")
	store, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "DELETE FROM schema_migrations WHERE version >= 40"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "DROP INDEX quality_gate_questions_fingerprint_idx"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "DROP TABLE quality_gate_questions"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	migrated, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	var columns int
	if err := migrated.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM pragma_table_info('quality_gate_questions') WHERE name IN ('fingerprint', 'allowed_answers_json', 'consumed_at')").Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if columns != 3 {
		t.Fatalf("quality_gate_questions columns = %d, want 3", columns)
	}
}

func TestMigrationFromV40AddsTicketDeliveryMergeCommit(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "workflow.db")
	store, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "DELETE FROM schema_migrations WHERE version >= 41"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "ALTER TABLE ticket_deliveries DROP COLUMN merge_commit"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	migrated, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	if !hasColumn(t, ctx, migrated.db, "ticket_deliveries", "merge_commit") {
		t.Fatal("migration did not add ticket_deliveries.merge_commit")
	}
}

func TestMigrationFromV42AddsMergeReadyObservationColumns(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "workflow.db")
	store, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "DELETE FROM schema_migrations WHERE version >= 43"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	for _, column := range []string{"validated_base_commit", "validated_head_commit"} {
		if _, err := db.ExecContext(ctx, "ALTER TABLE ticket_deliveries DROP COLUMN "+column); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	migrated, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	for _, column := range []string{"validated_base_commit", "validated_head_commit"} {
		if !hasColumn(t, ctx, migrated.db, "ticket_deliveries", column) {
			t.Fatalf("migration did not add ticket_deliveries.%s", column)
		}
	}
}

func TestMigrationFromV49RepairsDeliveredQuestions(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "workflow.db")
	snapshot := testSnapshot()
	db, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := snapshot.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	version, err := db.BeginActivation(ctx, snapshot, fingerprint, "revision-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkActive(ctx, version.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := db.QueueWorkflowInboxProjection(ctx, snapshot.Repository, now); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkTicketDelivered(ctx, version.ID, 1); err != nil {
		t.Fatal(err)
	}
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"needs_attention", "quality_gate", "closed_unmerged_impact"} {
		if err := ensureWorkflowQuestionTx(ctx, tx, snapshot.Repository, version.ID, 1, kind, "stale delivery recovery", now.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO plan_freezes(version_id, issue_id, reason, frozen_at) VALUES (?, ?, ?, ?)`, version.ID, int64(1), "pull request closed without merge", formatTimestamp(now)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.QueueWorkflowInboxProjection(ctx, snapshot.Repository, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	var beforeGeneration int64
	if err := db.db.QueryRowContext(ctx, `SELECT generation FROM workflow_inbox_projections WHERE repository = ?`, snapshot.Repository).Scan(&beforeGeneration); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, "DELETE FROM schema_migrations WHERE version >= 50"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	migrated, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	var repairedQuestions int
	if err := migrated.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM workflow_questions
WHERE version_id = ? AND issue_id = ? AND kind IN ('needs_attention', 'quality_gate', 'closed_unmerged_impact')
AND state = 'answered' AND answer = 'resolved by delivery' AND answered_at != ''`, version.ID, 1).Scan(&repairedQuestions); err != nil {
		t.Fatal(err)
	}
	if repairedQuestions != 2 {
		t.Fatalf("repaired delivered questions after migration = %d", repairedQuestions)
	}
	var preservedHumanGates int
	if err := migrated.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM workflow_questions
WHERE version_id = ? AND issue_id = ? AND kind = 'quality_gate' AND state = 'open'`, version.ID, 1).Scan(&preservedHumanGates); err != nil {
		t.Fatal(err)
	}
	if preservedHumanGates != 1 {
		t.Fatalf("open human gates after migration = %d, want 1", preservedHumanGates)
	}
	var remainingFreezes int
	if err := migrated.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM plan_freezes WHERE version_id = ? AND issue_id = ?`, version.ID, 1).Scan(&remainingFreezes); err != nil {
		t.Fatal(err)
	}
	if remainingFreezes != 0 {
		t.Fatalf("delivered ticket freezes after migration = %d", remainingFreezes)
	}
	var afterGeneration int64
	if err := migrated.db.QueryRowContext(ctx, `SELECT generation FROM workflow_inbox_projections WHERE repository = ?`, snapshot.Repository).Scan(&afterGeneration); err != nil {
		t.Fatal(err)
	}
	if afterGeneration != beforeGeneration+1 {
		t.Fatalf("Inbox projection generation after migration = %d, want %d", afterGeneration, beforeGeneration+1)
	}
}

func TestReopenWithoutMigrationPreservesVerifiedBackup(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "workflow.db")
	db, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	backupPath := dbPath + ".migration.bak"
	before, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("opening a current database overwrote its migration backup")
	}
}

func TestMigrationFromV29AddsRotationFencingColumns(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "workflow.db")
	store, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "DELETE FROM schema_migrations WHERE version >= 30"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	for _, column := range []string{"rotation_owner", "rotation_generation", "rotation_expires_at"} {
		if _, err := db.ExecContext(ctx, "ALTER TABLE gateway_runtime DROP COLUMN "+column); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	migrated, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	for _, column := range []string{"rotation_owner", "rotation_generation", "rotation_expires_at"} {
		if !hasColumn(t, ctx, migrated.db, "gateway_runtime", column) {
			t.Fatalf("migration did not add gateway_runtime.%s", column)
		}
	}
}

func TestMigrationFromV30AddsGitHubPollFailureKind(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "workflow.db")
	store, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "DELETE FROM schema_migrations WHERE version >= 31"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "ALTER TABLE github_poll_cursors DROP COLUMN failure_kind"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	backupPath := dbPath + ".migration.bak"
	if err := os.Remove(backupPath); err != nil {
		t.Fatal(err)
	}
	migrated, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	if !hasColumn(t, ctx, migrated.db, "github_poll_cursors", "failure_kind") {
		t.Fatal("migration did not add github_poll_cursors.failure_kind")
	}
	backup, err := sql.Open("sqlite", backupPath)
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	if hasColumn(t, ctx, backup, "github_poll_cursors", "failure_kind") {
		t.Fatal("migration backup includes the v31 GitHub poll failure column")
	}
}

func TestMigrationFromV31AddsGitHubPollRecoveryState(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "workflow.db")
	repository := "owner/repo"
	now := time.Now().UTC().Add(24 * time.Hour)
	store, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO github_poll_cursors(repository, consecutive_failures, failure_kind, recovery_state, recovery_plan_version_id, next_attempt_at, updated_at)
VALUES (?, 99, ?, ?, '', ?, ?)`, repository, GitHubPollFailurePreActivationInboxConflict, GitHubPollRecoveryAvailable, formatTimestamp(now), formatTimestamp(now)); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "DELETE FROM schema_migrations WHERE version >= 32"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "ALTER TABLE github_poll_cursors DROP COLUMN recovery_state"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	backupPath := dbPath + ".migration.bak"
	if err := os.Remove(backupPath); err != nil {
		t.Fatal(err)
	}
	migrated, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	if !hasColumn(t, ctx, migrated.db, "github_poll_cursors", "recovery_state") {
		t.Fatal("migration did not add github_poll_cursors.recovery_state")
	}
	cursor, err := migrated.GitHubPollCursor(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	if cursor.ConsecutiveFailures != 0 || cursor.FailureKind != GitHubPollFailureRetryable || cursor.RecoveryState != GitHubPollRecoveryConsumed {
		t.Fatalf("migrated recovery = %#v, want reset retry budget with safely consumed legacy provenance", cursor)
	}
	if cursor.NextAttemptAt.IsZero() || cursor.NextAttemptAt.After(time.Now().UTC()) {
		t.Fatalf("migrated next attempt = %v, want immediately ready", cursor.NextAttemptAt)
	}
	backup, err := sql.Open("sqlite", backupPath)
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	if hasColumn(t, ctx, backup, "github_poll_cursors", "recovery_state") {
		t.Fatal("migration backup includes the v32 GitHub poll recovery column")
	}
}

func TestMigrationFromV32AddsGitHubPollLeaseColumns(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "workflow.db")
	store, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "DELETE FROM schema_migrations WHERE version >= 33"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	for _, column := range []string{"lease_token", "lease_expires_at"} {
		if _, err := db.ExecContext(ctx, "ALTER TABLE github_poll_cursors DROP COLUMN "+column); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	backupPath := dbPath + ".migration.bak"
	if err := os.Remove(backupPath); err != nil {
		t.Fatal(err)
	}
	migrated, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	for _, column := range []string{"lease_token", "lease_expires_at"} {
		if !hasColumn(t, ctx, migrated.db, "github_poll_cursors", column) {
			t.Fatalf("migration did not add github_poll_cursors.%s", column)
		}
	}
}

func TestMigrationFromV33AddsBootstrapPlanProvenance(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "workflow.db")
	store, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "DELETE FROM schema_migrations WHERE version >= 34"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "ALTER TABLE github_poll_cursors DROP COLUMN recovery_plan_version_id"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	backupPath := dbPath + ".migration.bak"
	if err := os.Remove(backupPath); err != nil {
		t.Fatal(err)
	}
	migrated, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	if !hasColumn(t, ctx, migrated.db, "github_poll_cursors", "recovery_plan_version_id") {
		t.Fatal("migration did not add github_poll_cursors.recovery_plan_version_id")
	}
}

func TestMigrationFromV34QueuesAuthoritativeLegacyInboxProjection(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "workflow.db")
	snapshot := testSnapshot()
	store, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := snapshot.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	version, err := store.BeginActivation(ctx, snapshot, fingerprint, "revision-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkActive(ctx, version.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.QueueWorkflowInboxProjection(ctx, snapshot.Repository, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "DELETE FROM schema_migrations WHERE version >= 35"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "DROP TABLE workflow_inbox_projections"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE delivery_outbox
	SET request_json = json_remove(request_json, '$.inbox_projection_generation', '$.inbox_projection_version', '$.inbox_plan_version_id', '$.inbox_plan_version_ids'), uncertain = 1,
	    state = 'rejected', last_error = 'legacy rejection', completed_at = updated_at
	WHERE operation = 'project_workflow_inbox'`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO completed_plan_versions(version_id, completed_at) VALUES (?, ?)`, version.ID, formatTimestamp(time.Now().UTC())); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO plan_terminal_states(version_id, state, recorded_at) VALUES (?, ?, ?)`, version.ID, StateCompleted, formatTimestamp(time.Now().UTC())); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(dbPath + ".migration.bak"); err != nil {
		t.Fatal(err)
	}
	migrated, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	var generation int64
	var projectionVersion, planVersionIDs string
	if err := migrated.db.QueryRowContext(ctx, `SELECT generation, projection_version, plan_version_ids_json FROM workflow_inbox_projections WHERE repository = ?`, snapshot.Repository).Scan(&generation, &projectionVersion, &planVersionIDs); err != nil {
		t.Fatal(err)
	}
	emptyVersion, err := workflowInboxProjectionVersion(nil)
	if err != nil {
		t.Fatal(err)
	}
	if generation != 2 || projectionVersion != emptyVersion || planVersionIDs != "null" {
		t.Fatalf("migrated Inbox projection = %d/%q/%s, want generation 2 empty projection", generation, projectionVersion, planVersionIDs)
	}
	var corrections int
	if err := migrated.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM delivery_outbox
WHERE operation = 'project_workflow_inbox'
  AND json_extract(request_json, '$.repository') = ?
  AND json_extract(request_json, '$.inbox_projection_generation') = 2`, snapshot.Repository).Scan(&corrections); err != nil {
		t.Fatal(err)
	}
	if corrections != 1 {
		t.Fatalf("queued empty Inbox corrections = %d, want 1", corrections)
	}
	recoverableKeys, err := migrated.RecoverableUncertainInboxDeliveryKeys(ctx, snapshot.Repository)
	if err != nil {
		t.Fatal(err)
	}
	if len(recoverableKeys) != 1 {
		t.Fatalf("recoverable legacy Inbox deliveries = %v, want one key", recoverableKeys)
	}
	var correctionKey string
	if err := migrated.db.QueryRowContext(ctx, `SELECT idempotency_key FROM delivery_outbox
WHERE operation = 'project_workflow_inbox'
  AND json_extract(request_json, '$.repository') = ?
  AND json_extract(request_json, '$.inbox_projection_generation') = 2`, snapshot.Repository).Scan(&correctionKey); err != nil {
		t.Fatal(err)
	}
	if _, err := migrated.ClaimDeliveryOutbox(ctx, correctionKey, time.Now().UTC()); !errors.Is(err, ErrInboxDeliveryPending) {
		t.Fatalf("legacy uncertain Inbox fence = %v, want pending", err)
	}
	recoveryQuestionID, err := migrated.UncertainInboxDeliveryRecoveryQuestionID(ctx, snapshot.Repository, recoverableKeys[0])
	if err != nil {
		t.Fatal(err)
	}
	questions, err := migrated.WorkflowInboxQuestions(ctx, snapshot.Repository)
	if err != nil {
		t.Fatal(err)
	}
	if len(questions) != 1 || questions[0].ID != recoveryQuestionID || questions[0].RootNumber != 0 || len(questions[0].PlanNumbers) != 0 {
		t.Fatalf("legacy repository-scoped recovery question = %#v", questions)
	}
	if _, err := migrated.RecoverUncertainInboxDelivery(ctx, snapshot.Repository, recoverableKeys[0], recoveryQuestionID, "retry", time.Now().UTC()); err != nil {
		t.Fatalf("recover discoverable legacy Inbox delivery: %v", err)
	}
	recovered, err := migrated.DeliveryOutbox(ctx, recoverableKeys[0])
	if err != nil {
		t.Fatal(err)
	}
	if recovered.State != OutboxPending || !recovered.Uncertain {
		t.Fatalf("recovered legacy Inbox delivery = %#v", recovered)
	}
	t.Logf("migration restored legacy recovery key %s with question %s; authoritative correction generation=%d remained ordered behind it", recoverableKeys[0], recoveryQuestionID, generation)
}

func TestMigrationBackupCanBeRestored(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "workflow.db")
	db, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(dbPath + ".migration.bak")
	if err != nil {
		t.Fatal(err)
	}
	restoredPath := filepath.Join(filepath.Dir(dbPath), "restored.db")
	if err := os.WriteFile(restoredPath, backup, 0o600); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(ctx, restoredPath)
	if err != nil {
		t.Fatalf("open restored migration backup: %v", err)
	}
	if err := restored.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationFromV17BacksUpBeforeAddingRuntimeRetentionColumns(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "workflow.db")
	store, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "DELETE FROM schema_migrations WHERE version >= 18"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "ALTER TABLE ticket_sessions DROP COLUMN workspace_reclaimed_at"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "ALTER TABLE ticket_sessions DROP COLUMN delivery_retry_pending"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()

	backup, err := sql.Open("sqlite", dbPath+".migration.bak")
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	if hasColumn(t, ctx, backup, "ticket_sessions", "delivery_retry_pending") {
		t.Fatal("migration backup includes the v18 delivery retry column")
	}
	if hasColumn(t, ctx, backup, "ticket_sessions", "workspace_reclaimed_at") {
		t.Fatal("migration backup includes the v19 workspace retention column")
	}
	if !hasColumn(t, ctx, migrated.db, "ticket_sessions", "delivery_retry_pending") {
		t.Fatal("migration did not add the delivery retry column")
	}
	if !hasColumn(t, ctx, migrated.db, "ticket_sessions", "workspace_reclaimed_at") {
		t.Fatal("migration did not add the workspace retention column")
	}
}

func TestMigrationFromV22BacksUpBeforeAddingReplacementIdentity(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "workflow.db")
	store, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "DELETE FROM schema_migrations WHERE version >= 23"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "ALTER TABLE replacement_tickets DROP COLUMN replacement_version_id"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "ALTER TABLE replacement_tickets DROP COLUMN replacement_issue_id"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	backup, err := sql.Open("sqlite", dbPath+".migration.bak")
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	if hasColumn(t, ctx, backup, "replacement_tickets", "replacement_version_id") || hasColumn(t, ctx, backup, "replacement_tickets", "replacement_issue_id") {
		t.Fatal("migration backup includes v23 replacement identity columns")
	}
	if !hasColumn(t, ctx, migrated.db, "replacement_tickets", "replacement_version_id") || !hasColumn(t, ctx, migrated.db, "replacement_tickets", "replacement_issue_id") {
		t.Fatal("migration did not add replacement identity columns")
	}
}

func hasColumn(t *testing.T, ctx context.Context, db *sql.DB, table, column string) bool {
	t.Helper()
	rows, err := db.QueryContext(ctx, "SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		if name == column {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return false
}

func TestBeginActivationRejectsChangedImmutablePlan(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	snapshot := testSnapshot()
	fingerprint, err := snapshot.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginActivation(ctx, snapshot, fingerprint, "r1"); err != nil {
		t.Fatal(err)
	}
	snapshot.Children[0].Title = "changed contract"
	changed, err := snapshot.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginActivation(ctx, snapshot, changed, "r2"); err == nil {
		t.Fatal("BeginActivation() succeeded for changed immutable plan")
	}
}

func testSnapshot() plan.Snapshot {
	return plan.Snapshot{
		Repository: "owner/repo",
		Root:       plan.Issue{ID: 100, Number: 10, Title: "Plan", Labels: []string{plan.PlanLabel}},
		Children: []plan.Issue{
			{ID: 1, Number: 11, Title: "first", Labels: []string{plan.TicketLabel}, State: "open"},
			{ID: 2, Number: 12, Title: "second", Labels: []string{plan.TicketLabel}, State: "open"},
		},
		BlockedBy: map[int64][]plan.Issue{2: {{ID: 1, Number: 11, Labels: []string{plan.TicketLabel}, State: "open"}}},
	}
}
