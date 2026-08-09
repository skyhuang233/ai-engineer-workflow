// Package startup holds host-process guards acquired before the Control Plane opens SQLite.
package startup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

var ErrAlreadyRunning = errors.New("Control Plane is already running")

type Lock struct{ file *os.File }

func AcquireLock(databasePath string) (*Lock, error) {
	if databasePath == "" || databasePath == ":memory:" {
		return nil, errors.New("Control Plane lock requires a database path")
	}
	file, err := os.OpenFile(filepath.Clean(databasePath)+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open Control Plane lock: %w", err)
	}
	if err := lockFile(file); err != nil {
		_ = file.Close()
		if isLockConflict(err) {
			return nil, ErrAlreadyRunning
		}
		return nil, fmt.Errorf("lock Control Plane: %w", err)
	}
	return &Lock{file: file}, nil
}

func (l *Lock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := errors.Join(unlockFile(l.file), l.file.Close())
	l.file = nil
	return err
}
