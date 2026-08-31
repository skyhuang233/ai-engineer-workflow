//go:build darwin

package controlplane

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const launchdLabel = "com.skyhuang233.agent-workflow.control-plane"

type LaunchdExecutor interface {
	Run(context.Context, string, ...string) ([]byte, error)
}
type OSLaunchdExecutor struct{}

func (OSLaunchdExecutor) Run(ctx context.Context, command string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, command, args...).CombinedOutput()
}

type LaunchdService struct {
	Options  NativeServiceOptions
	Executor LaunchdExecutor
}

func NewNativeService(options NativeServiceOptions) (NativeService, error) {
	if err := options.validate(); err != nil {
		return nil, err
	}
	return LaunchdService{Options: options}, nil
}

func (s LaunchdService) Present(ctx context.Context) (bool, error) {
	if err := s.Options.validate(); err != nil {
		return false, err
	}
	executor := s.Executor
	if executor == nil {
		executor = OSLaunchdExecutor{}
	}
	output, err := executor.Run(ctx, "launchctl", "print", "gui/"+strconv.Itoa(os.Getuid())+"/"+launchdLabel)
	if err == nil {
		return true, nil
	}
	if strings.Contains(strings.ToLower(string(output)), "could not find service") {
		return false, nil
	}
	return false, fmt.Errorf("query native Control Plane service: %w (%s)", err, strings.TrimSpace(string(output)))
}

func (s LaunchdService) Register(ctx context.Context) error {
	if err := s.Options.validate(); err != nil {
		return err
	}
	executor := s.Executor
	if executor == nil {
		executor = OSLaunchdExecutor{}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve user LaunchAgents directory: %w", err)
	}
	directory := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	path := filepath.Join(directory, launchdLabel+".plist")
	if err := writeNewLaunchdPlist(path, launchdPlist(s.Options)); err != nil {
		return err
	}
	output, err := executor.Run(ctx, "launchctl", "bootstrap", "gui/"+strconv.Itoa(os.Getuid()), path)
	if err != nil {
		return fmt.Errorf("register native Control Plane service: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func writeNewLaunchdPlist(path, content string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.WriteString(content)
	return err
}

func launchdPlist(options NativeServiceOptions) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>` + launchdLabel + `</string>
<key>ProgramArguments</key><array><string>` + xmlEscape(options.Executable) + `</string><string>watch-service</string><string>--workflow-home</string><string>` + xmlEscape(options.WorkflowHome) + `</string></array>
<key>KeepAlive</key><true/>
</dict></plist>`
}

var _ NativeService = LaunchdService{}
