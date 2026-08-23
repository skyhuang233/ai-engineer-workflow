//go:build windows

package controlplane

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"golang.org/x/sys/windows"
)

func RequireCurrentUserProcess(pid int) error {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("open process owner: %w", err)
	}
	defer windows.CloseHandle(handle)
	var processToken windows.Token
	if err := windows.OpenProcessToken(handle, windows.TOKEN_QUERY, &processToken); err != nil {
		return fmt.Errorf("open process token: %w", err)
	}
	defer processToken.Close()
	processUser, err := processToken.GetTokenUser()
	if err != nil {
		return fmt.Errorf("read process user: %w", err)
	}
	currentToken, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return fmt.Errorf("open current process token: %w", err)
	}
	defer currentToken.Close()
	currentUser, err := currentToken.GetTokenUser()
	if err != nil {
		return fmt.Errorf("read current process user: %w", err)
	}
	if !processUser.User.Sid.Equals(currentUser.User.Sid) {
		return errors.New("Control Plane process belongs to a different user")
	}
	return nil
}

func OSProcessIdentity(pid int) (time.Time, bool, error) {
	if pid <= 0 {
		return time.Time{}, false, nil
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("open recorded process: %w", err)
	}
	defer windows.CloseHandle(handle)
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &created, &exited, &kernel, &user); err != nil {
		return time.Time{}, false, fmt.Errorf("read recorded process start identity: %w", err)
	}
	return time.Unix(0, created.Nanoseconds()).UTC(), true, nil
}

func LaunchDetached(executable string, args []string, stdoutPath, stderrPath string) (int, error) {
	stdout, err := os.OpenFile(stdoutPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	defer stdout.Close()
	stderr, err := os.OpenFile(stderrPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	defer stderr.Close()
	command := exec.Command(executable, args...)
	command.Stdout = stdout
	command.Stderr = stderr
	command.Stdin = nil
	command.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS | windows.CREATE_NO_WINDOW, HideWindow: true}
	if err := command.Start(); err != nil {
		return 0, fmt.Errorf("start detached Control Plane: %w", err)
	}
	pid := command.Process.Pid
	if err := command.Process.Release(); err != nil {
		return 0, fmt.Errorf("release detached Control Plane process handle: %w", err)
	}
	return pid, nil
}
