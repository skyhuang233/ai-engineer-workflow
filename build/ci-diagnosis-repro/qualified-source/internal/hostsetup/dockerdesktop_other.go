//go:build !windows

package hostsetup

import (
	"context"
	"errors"
)

type WindowsDockerDesktopHost struct{}

func (WindowsDockerDesktopHost) InstalledVersion(context.Context) (string, error) {
	return "", errors.New("Docker Desktop setup supports Windows only")
}
func (WindowsDockerDesktopHost) Download(context.Context, string, string) error {
	return errors.New("Docker Desktop setup supports Windows only")
}
func (WindowsDockerDesktopHost) InstallElevated(context.Context, string) error {
	return errors.New("Docker Desktop setup supports Windows only")
}
func (WindowsDockerDesktopHost) Start(context.Context) error {
	return errors.New("Docker Desktop setup supports Windows only")
}
func (WindowsDockerDesktopHost) EngineReady(context.Context) error {
	return errors.New("Docker Desktop setup supports Windows only")
}
