package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestActiveWorkerReleaseIsAtomicAndPersistsAcrossRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "workflow.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	first := WorkerRelease{
		Version: "0.1.0", SourceCommit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ImageDigest:  "ghcr.io/owner/worker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		ManifestJSON: `{"schema_version":1}`, VerifiedAt: time.Date(2026, 7, 31, 1, 0, 0, 0, time.UTC),
	}
	if err := db.ActivateWorkerRelease(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	active, err := db.ActiveWorkerRelease(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if active.ImageDigest != first.ImageDigest || active.SourceCommit != first.SourceCommit {
		t.Fatalf("active release = %#v", active)
	}
}
