//go:build windows

package hostsetup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

type WindowsDockerDesktopHost struct{ Client *http.Client }

func (WindowsDockerDesktopHost) InstalledVersion(context.Context) (string, error) {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\Docker Desktop`, registry.QUERY_VALUE)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer key.Close()
	version, _, err := key.GetStringValue("DisplayVersion")
	return strings.TrimSpace(version), err
}
func (h WindowsDockerDesktopHost) Download(ctx context.Context, url, path string) error {
	client := h.Client
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Docker Desktop download returned HTTP %d", response.StatusCode)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(file, io.LimitReader(response.Body, 2<<30))
	return errorsJoin(copyErr, file.Sync(), file.Close())
}
func (WindowsDockerDesktopHost) InstallElevated(ctx context.Context, path string) error {
	// Arguments after -Command are not a reliable $args binding for a command
	// string. Pass the exact path through a dedicated environment variable so
	// spaces survive both the PowerShell and UAC boundaries.
	command := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", dockerDesktopInstallerCommand())
	command.Env = dockerDesktopEnvironment(os.Environ(), dockerDesktopInstallerEnvironment, path)
	return command.Run()
}
func (WindowsDockerDesktopHost) Start(ctx context.Context) error {
	path := filepath.Join(os.Getenv("ProgramFiles"), "Docker", "Docker", "Docker Desktop.exe")
	command := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", dockerDesktopStartCommand())
	command.Env = dockerDesktopEnvironment(os.Environ(), dockerDesktopExecutableEnvironment, path)
	return command.Run()
}
func (WindowsDockerDesktopHost) EngineReady(ctx context.Context) error {
	output, err := exec.CommandContext(ctx, "docker", "info", "--format", "{{.OSType}}/{{.Architecture}}").CombinedOutput()
	if err != nil {
		return err
	}
	value := strings.TrimSpace(string(output))
	if value != "linux/x86_64" && value != "linux/amd64" {
		return fmt.Errorf("Docker engine is %s", value)
	}
	return nil
}
func errorsJoin(values ...error) error { return errors.Join(values...) }
