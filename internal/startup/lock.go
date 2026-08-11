// Package startup holds host-process guards acquired before the Control Plane opens SQLite.
package startup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

var ErrAlreadyRunning = errors.New("Control Plane is already running")

type Lock struct {
	file *os.File
	held []*os.File
}

func AcquireLock(databasePath string) (*Lock, error) {
	identity, err := DatabaseIdentity(databasePath)
	if err != nil {
		return nil, err
	}
	if identity == "" {
		return nil, errors.New("Control Plane lock requires a database path")
	}
	file, err := openLockFile(identity + ".lock")
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
	identity, err := DatabaseIdentity(databasePath)
	if err != nil {
		return nil, err
	}
	if identity == "" {
		return nil, errors.New("database restore barrier requires a database path")
	}
	turnstile, err := openLockFile(identity + ".restore.intent.lock")
	if err != nil {
		return nil, err
	}
	if err := acquireFileLock(ctx, turnstile, exclusive, "database restore turnstile"); err != nil {
		_ = turnstile.Close()
		return nil, err
	}
	file, err := openLockFile(identity + ".restore.lock")
	if err != nil {
		_ = unlockFile(turnstile)
		_ = turnstile.Close()
		return nil, err
	}
	if err := acquireFileLock(ctx, file, exclusive, "database restore barrier"); err != nil {
		_ = file.Close()
		_ = unlockFile(turnstile)
		_ = turnstile.Close()
		return nil, err
	}
	if exclusive {
		return &Lock{file: file, held: []*os.File{turnstile}}, nil
	}
	turnstileErr := errors.Join(unlockFile(turnstile), turnstile.Close())
	if turnstileErr != nil {
		_ = unlockFile(file)
		_ = file.Close()
		return nil, turnstileErr
	}
	return &Lock{file: file}, nil
}

func acquireFileLock(ctx context.Context, file *os.File, exclusive bool, description string) error {
	for {
		if err := tryLockFile(file, exclusive); err == nil {
			return nil
		} else if !isLockConflict(err) {
			return fmt.Errorf("lock %s: %w", description, err)
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
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
	for i := len(l.held) - 1; i >= 0; i-- {
		err = errors.Join(err, unlockFile(l.held[i]), l.held[i].Close())
	}
	l.file = nil
	l.held = nil
	return err
}
