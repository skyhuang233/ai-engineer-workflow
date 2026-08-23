package workflowhome

import (
	"errors"
	"strings"
)

// PathStore is the current-user PATH persistence boundary. The Windows
// implementation stores this value in HKCU\Environment; tests use an
// in-memory store so they never mutate the developer's environment.
type PathStore interface {
	Load() (string, error)
	Save(string) error
}

func PersistPath(store PathStore, bin string) error {
	if store == nil || strings.TrimSpace(bin) == "" {
		return errors.New("current-user PATH store and Workflow bin are required")
	}
	current, err := store.Load()
	if err != nil {
		return err
	}
	reconciled := ReconcilePath(current, bin)
	if reconciled == current {
		return nil
	}
	return store.Save(reconciled)
}

func PathIsReconciled(store PathStore, bin string) (bool, error) {
	if store == nil || strings.TrimSpace(bin) == "" {
		return false, errors.New("current-user PATH store and Workflow bin are required")
	}
	current, err := store.Load()
	if err != nil {
		return false, err
	}
	return ReconcilePath(current, bin) == current, nil
}
