package startup

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func AcquireWorkflowHomeLock(workflowHome string) (*Lock, error) {
	identity, err := canonicalLocalIdentity(workflowHome)
	if err != nil {
		return nil, err
	}
	return acquireNamedLock(filepath.Join(identity, "state", "setup.lock"), "Workflow Home")
}

// AcquireControlPlaneLaunchLock serializes public serve invocations while a
// foreground child acquires the distinct lifetime lock and becomes healthy.
func AcquireControlPlaneLaunchLock(workflowHome string) (*Lock, error) {
	identity, err := canonicalLocalIdentity(workflowHome)
	if err != nil {
		return nil, err
	}
	return acquireNamedLock(filepath.Join(identity, "state", "control-plane-launch.lock"), "Control Plane launch")
}

// AcquireControlPlaneRuntimeLock is held by the foreground child for its full
// lifetime. It is not a service registration or restart authority.
func AcquireControlPlaneRuntimeLock(workflowHome string) (*Lock, error) {
	identity, err := canonicalLocalIdentity(workflowHome)
	if err != nil {
		return nil, err
	}
	return acquireNamedLock(filepath.Join(identity, "state", "control-plane-runtime.lock"), "Control Plane runtime")
}

func AcquireRepositoryLock(workflowHome, repository string) (*Lock, error) {
	home, err := canonicalLocalIdentity(workflowHome)
	if err != nil {
		return nil, err
	}
	repo, err := canonicalLocalIdentity(repository)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(strings.ToLower(repo)))
	return acquireNamedLock(filepath.Join(home, "state", "locks", "repositories", hex.EncodeToString(digest[:])+".lock"), "repository setup")
}

func canonicalLocalIdentity(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("lock identity must be an absolute path")
	}
	value, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(value, `\\`) {
		return "", errors.New("lock identity must be local")
	}
	return value, nil
}

func acquireNamedLock(path, description string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := openLockFile(path)
	if err != nil {
		return nil, err
	}
	if err := tryLockFile(file, true); err != nil {
		_ = file.Close()
		if isLockConflict(err) {
			return nil, ErrAlreadyRunning
		}
		return nil, fmt.Errorf("lock %s: %w", description, err)
	}
	return &Lock{file: file}, nil
}
