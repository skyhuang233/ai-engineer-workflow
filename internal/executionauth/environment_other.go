//go:build !windows

package executionauth

import "errors"

func ReloadCurrentUser() (Selection, error) {
	return Selection{}, errors.New("Worker execution authentication persistence is supported on Windows only")
}

func CommitCurrentUser(Selection) error {
	return errors.New("Worker execution authentication persistence is supported on Windows only")
}
