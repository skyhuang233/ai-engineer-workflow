//go:build windows

package workflowhome

import (
	"errors"
	"os"

	"golang.org/x/sys/windows/registry"
)

type currentUserPathStore struct{}

func (currentUserPathStore) Load() (string, error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, `Environment`, registry.QUERY_VALUE)
	if err != nil {
		return "", err
	}
	defer key.Close()
	value, _, err := key.GetStringValue("Path")
	if errors.Is(err, registry.ErrNotExist) {
		return "", nil
	}
	return value, err
}

func (currentUserPathStore) Save(value string) error {
	key, err := registry.OpenKey(registry.CURRENT_USER, `Environment`, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	return key.SetStringValue("Path", value)
}

// PersistCurrentUserPath writes only the current user's PATH and also updates
// this process so subsequent setup effects can discover the installed shim.
func PersistCurrentUserPath(bin string) error {
	if err := PersistPath(currentUserPathStore{}, bin); err != nil {
		return err
	}
	return os.Setenv("PATH", ReconcilePath(os.Getenv("PATH"), bin))
}

func CurrentUserPathIsReconciled(bin string) (bool, error) {
	return PathIsReconciled(currentUserPathStore{}, bin)
}
