package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestHealthVerifiesProductionSQLitePragmasAndLocking(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "workflow.db")
	db, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	health, err := db.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if health.JournalMode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", health.JournalMode)
	}
	if health.Synchronous != 2 {
		t.Fatalf("synchronous = %d, want 2 (FULL)", health.Synchronous)
	}
	if !health.ForeignKeys {
		t.Fatal("foreign_keys = false, want true")
	}
	if health.Integrity != "ok" {
		t.Fatalf("integrity_check = %q, want ok", health.Integrity)
	}
	if !health.WriteLocking {
		t.Fatal("write-lock probe did not observe SQLite serialization")
	}
}
