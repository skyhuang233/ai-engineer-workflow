package store

import (
	"context"
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
