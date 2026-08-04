package store

import (
	"context"
	"errors"
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
		ImageReference: "ghcr.io/owner/worker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		ManifestJSON:   `{"schema_version":1}`, VerifiedAt: time.Date(2026, 7, 31, 1, 0, 0, 0, time.UTC),
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
	if active.ImageReference != first.ImageReference || active.SourceCommit != first.SourceCommit {
		t.Fatalf("active release = %#v", active)
	}
}

func TestWorkerReleaseDigestCannotBeRewritten(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	release := WorkerRelease{
		Version: "0.1.0", SourceCommit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ImageReference: "ghcr.io/owner/worker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		ManifestJSON:   `{"schema_version":1}`, VerifiedAt: time.Now().UTC(),
	}
	if err := db.ActivateWorkerRelease(ctx, release); err != nil {
		t.Fatal(err)
	}
	release.SourceCommit = "cccccccccccccccccccccccccccccccccccccccc"
	if err := db.ActivateWorkerRelease(ctx, release); err == nil {
		t.Fatal("rewrote an immutable Worker Release digest")
	}
}

func TestWorkerReleaseActivationFenceRejectsAChangedActiveImage(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	first := WorkerRelease{
		Version: "0.1.0", SourceCommit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ImageReference: "ghcr.io/owner/worker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		ManifestJSON:   `{"schema_version":1}`, VerifiedAt: time.Now().UTC(),
	}
	if err := db.ActivateWorkerRelease(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.SourceCommit = "cccccccccccccccccccccccccccccccccccccccc"
	second.ImageReference = "ghcr.io/owner/worker@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	if err := db.ActivateWorkerRelease(ctx, second); err != nil {
		t.Fatal(err)
	}
	third := first
	third.ImageReference = "ghcr.io/owner/worker@sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	if err := db.ActivateWorkerReleaseFenced(ctx, third, first.ImageReference); !errors.Is(err, ErrFencingConflict) {
		t.Fatalf("stale activation fence = %v", err)
	}
}
