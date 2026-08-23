//go:build !windows

package controlplane

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func RequireCurrentUserProcess(pid int) error {
	info, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
	if err != nil {
		return fmt.Errorf("inspect process owner: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return errors.New("Control Plane process belongs to a different user")
	}
	return nil
}

func OSProcessIdentity(pid int) (time.Time, bool, error) {
	if pid <= 0 {
		return time.Time{}, false, nil
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return time.Time{}, false, nil
	}
	if err := process.Signal(syscall.Signal(0)); err != nil {
		if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, err
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return time.Time{}, true, fmt.Errorf("read process start identity: %w", err)
	}
	closing := strings.LastIndexByte(string(stat), ')')
	if closing < 0 {
		return time.Time{}, true, errors.New("invalid process stat identity")
	}
	fields := strings.Fields(string(stat)[closing+1:])
	if len(fields) <= 19 {
		return time.Time{}, true, errors.New("invalid process stat identity")
	}
	startTicks, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return time.Time{}, true, err
	}
	bootStat, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}, true, err
	}
	var bootSeconds int64
	for _, line := range strings.Split(string(bootStat), "\n") {
		if strings.HasPrefix(line, "btime ") {
			bootSeconds, err = strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "btime ")), 10, 64)
			break
		}
	}
	if err != nil || bootSeconds == 0 {
		return time.Time{}, true, errors.New("process boot identity is unavailable")
	}
	ticksPerSecond := int64(100)
	if output, commandErr := exec.Command("getconf", "CLK_TCK").Output(); commandErr == nil {
		if value, parseErr := strconv.ParseInt(strings.TrimSpace(string(output)), 10, 64); parseErr == nil && value > 0 {
			ticksPerSecond = value
		}
	}
	seconds, remainder := startTicks/ticksPerSecond, startTicks%ticksPerSecond
	return time.Unix(bootSeconds+seconds, remainder*int64(time.Second)/ticksPerSecond).UTC(), true, nil
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
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return 0, fmt.Errorf("start detached Control Plane: %w", err)
	}
	pid := command.Process.Pid
	if err := command.Process.Release(); err != nil {
		return 0, err
	}
	return pid, nil
}
