//go:build !windows

package credential

import (
	"context"
	"errors"
)

type unsupportedStore struct{}

func NewStore() Store { return unsupportedStore{} }

func (unsupportedStore) Get(context.Context, string) (string, error) {
	return "", errors.New("Windows Credential Manager is required")
}

func (unsupportedStore) Set(context.Context, string, string) error {
	return errors.New("Windows Credential Manager is required")
}
