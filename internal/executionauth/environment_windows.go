//go:build windows

package executionauth

import (
	"errors"

	"golang.org/x/sys/windows/registry"
)

type currentUserEnvironmentStore struct{}

func (currentUserEnvironmentStore) Load(name string) (string, error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, `Environment`, registry.QUERY_VALUE)
	if err != nil {
		return "", err
	}
	defer key.Close()
	value, _, err := key.GetStringValue(name)
	if errors.Is(err, registry.ErrNotExist) {
		return "", nil
	}
	return value, err
}

func (currentUserEnvironmentStore) Save(name, value string) error {
	key, err := registry.OpenKey(registry.CURRENT_USER, `Environment`, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	return key.SetStringValue(name, value)
}

func ReloadCurrentUser() (Selection, error) { return Reload(currentUserEnvironmentStore{}) }
func CommitCurrentUser(selection Selection) error {
	return Commit(currentUserEnvironmentStore{}, selection)
}
