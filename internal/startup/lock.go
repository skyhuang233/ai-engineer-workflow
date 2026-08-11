// Package startup holds host-process guards acquired before the Control Plane opens SQLite.
package startup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

var ErrAlreadyRunning = errors.New("Control Plane is already running")

type Lock struct{ file *os.File }

func AcquireLock(databasePath string) (*Lock, error) {
	if databasePath == "" || databasePath == ":memory:" {
		return nil, errors.New("Control Plane lock requires a database path")
	}
	file, err := openLockFile(filepath.Clean(databasePath) + ".lock")
	if err != nil {
		return nil, err
	}
	if err := tryLockFile(file, true); err != nil {
		_ = file.Close()
		if isLockConflict(err) {
			return nil, ErrAlreadyRunning
		}
		return nil, fmt.Errorf("lock Control Plane: %w", err)
	}
	return &Lock{file: file}, nil
}

func AcquireDatabaseAccess(ctx context.Context, databasePath string) (*Lock, error) {
	return acquireDatabaseBarrier(ctx, databasePath, false)
}

func AcquireRestoreBarrier(ctx context.Context, databasePath string) (*Lock, error) {
	return acquireDatabaseBarrier(ctx, databasePath, true)
}

func acquireDatabaseBarrier(ctx context.Context, databasePath string, exclusive bool) (*Lock, error) {
	if databasePath == "" || databasePath == ":memory:" {
		return nil, errors.New("database restore barrier requires a database path")
	}
	file, err := openLockFile(filepath.Clean(databasePath) + ".restore.lock")
	if err != nil {
		return nil, err
	}
	for {
		if err := tryLockFile(file, exclusive); err == nil {
			return &Lock{file: file}, nil
		} else if !isLockConflict(err) {
			_ = file.Close()
			return nil, fmt.Errorf("lock database restore barrier: %w", err)
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = file.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func openLockFile(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open Control Plane lock: %w", err)
	}
	return file, nil
}

func (l *Lock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := errors.Join(unlockFile(l.file), l.file.Close())
	l.file = nil
	return err
}
