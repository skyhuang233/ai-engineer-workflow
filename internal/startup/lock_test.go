package startup

import (
	"errors"
	"path/filepath"
	"testing"
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
