package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/skyhuang233/workflow/internal/plan"
)

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
