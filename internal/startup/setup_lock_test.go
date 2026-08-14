package startup

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestWorkflowHomeAndRepositoryLocksFenceOnlyMatchingIdentity(t *testing.T) {
	home := t.TempDir()
	first, err := AcquireWorkflowHomeLock(home)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := AcquireWorkflowHomeLock(home); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second home lock = %v", err)
	}
	repoOne, err := AcquireRepositoryLock(home, filepath.Join(t.TempDir(), "repo-one"))
	if err != nil {
		t.Fatal(err)
	}
	defer repoOne.Close()
	repoTwo, err := AcquireRepositoryLock(home, filepath.Join(t.TempDir(), "repo-two"))
	if err != nil {
		t.Fatalf("independent repository lock = %v", err)
	}
	defer repoTwo.Close()
}
