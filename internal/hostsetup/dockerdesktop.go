package hostsetup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type DockerDesktopContract struct{ Version, InstallerURL, WindowsAMD64SHA256 string }
type DockerDesktopHost interface {
	InstalledVersion(context.Context) (string, error)
	Download(context.Context, string, string) error
	InstallElevated(context.Context, string) error
	Start(context.Context) error
	EngineReady(context.Context) error
}

func EnsureDockerDesktop(ctx context.Context, contract DockerDesktopContract, host DockerDesktopHost, temporaryRoot string, timeout time.Duration) error {
	if host == nil || contract.Version == "" || !strings.HasPrefix(contract.InstallerURL, "https://") || len(contract.WindowsAMD64SHA256) != 64 {
		return errors.New("complete verified Docker Desktop contract is required")
	}
	installed, err := host.InstalledVersion(ctx)
	if err != nil {
		return err
	}
	if installed == contract.Version {
		return waitEngine(ctx, host, timeout)
	}
	if err := os.MkdirAll(temporaryRoot, 0o700); err != nil {
		return err
	}
	installer := filepath.Join(temporaryRoot, "DockerDesktopInstaller.exe")
	if err := host.Download(ctx, contract.InstallerURL, installer); err != nil {
		return fmt.Errorf("download Docker Desktop: %w", err)
	}
	defer os.Remove(installer)
	data, err := os.ReadFile(installer)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != strings.ToLower(contract.WindowsAMD64SHA256) {
		return errors.New("Docker Desktop installer checksum mismatch")
	}
	if err := host.InstallElevated(ctx, installer); err != nil {
		return fmt.Errorf("install Docker Desktop: %w", err)
	}
	installed, err = host.InstalledVersion(ctx)
	if err != nil || installed != contract.Version {
		return fmt.Errorf("Docker Desktop version readback = %q, want %q: %w", installed, contract.Version, err)
	}
	if err := host.Start(ctx); err != nil {
		return err
	}
	return waitEngine(ctx, host, timeout)
}
func waitEngine(ctx context.Context, host DockerDesktopHost, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	deadlineCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if err := host.EngineReady(deadlineCtx); err == nil {
			return nil
		}
		select {
		case <-deadlineCtx.Done():
			return fmt.Errorf("wait for Docker Desktop Linux engine: %w", deadlineCtx.Err())
		case <-ticker.C:
		}
	}
}
