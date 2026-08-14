//go:build !windows

package workflowhome

import "errors"

func PersistCurrentUserPath(string) error {
	return errors.New("current-user PATH persistence is supported on Windows only")
}

func CurrentUserPathIsReconciled(string) (bool, error) {
	return false, errors.New("current-user PATH persistence is supported on Windows only")
}
