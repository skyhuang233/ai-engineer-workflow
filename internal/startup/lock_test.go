package startup

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestAcquireLockRejectsConcurrentControlPlaneAndReleasesOnClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workflow.db")
	first, err := AcquireLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireLock(path); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("concurrent lock error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := AcquireLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreBarrierDrainsDatabaseAccessAndBlocksNewAccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workflow.db")
	first, err := AcquireDatabaseAccess(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := AcquireDatabaseAccess(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	blocked, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := AcquireRestoreBarrier(blocked, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("restore barrier while database is active = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	restore, err := AcquireRestoreBarrier(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	blocked, cancel = context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := AcquireDatabaseAccess(blocked, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("database access during restore = %v", err)
	}
	if err := restore.Close(); err != nil {
		t.Fatal(err)
	}
	access, err := AcquireDatabaseAccess(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := access.Close(); err != nil {
		t.Fatal(err)
	}
}
