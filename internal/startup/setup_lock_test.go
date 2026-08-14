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

func TestControlPlaneLaunchAndRuntimeLocksAreIndependentAndExclusive(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	launch, err := AcquireControlPlaneLaunchLock(home)
	if err != nil {
		t.Fatal(err)
	}
	defer launch.Close()
	if _, err := AcquireControlPlaneLaunchLock(home); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second launch lock = %v", err)
	}
	runtime, err := AcquireControlPlaneRuntimeLock(home)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if _, err := AcquireControlPlaneRuntimeLock(home); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second runtime lock = %v", err)
	}
}
