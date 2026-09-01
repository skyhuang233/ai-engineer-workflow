//go:build windows

package controlplane

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

type TaskSchedulerExecutor interface {
	Run(context.Context, string, ...string) ([]byte, error)
}
type OSTaskSchedulerExecutor struct{}

func (OSTaskSchedulerExecutor) Run(ctx context.Context, command string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, command, args...).CombinedOutput()
}

type TaskSchedulerService struct {
	Options    NativeServiceOptions
	Executor   TaskSchedulerExecutor
	CreateTemp func(string, string) (*os.File, error)
	Remove     func(string) error
}

func NewNativeService(options NativeServiceOptions) (NativeService, error) {
	if err := options.validate(); err != nil {
		return nil, err
	}
	return TaskSchedulerService{Options: options}, nil
}

func (s TaskSchedulerService) Present(ctx context.Context) (bool, error) {
	if err := s.Options.validate(); err != nil {
		return false, err
	}
	executor := s.Executor
	if executor == nil {
		executor = OSTaskSchedulerExecutor{}
	}
	output, err := executor.Run(ctx, "schtasks.exe", "/query", "/tn", NativeServiceName)
	if err == nil {
		return true, nil
	}
	if strings.Contains(strings.ToLower(string(output)), "cannot find the file specified") || strings.Contains(strings.ToLower(string(output)), "does not exist") {
		return false, nil
	}
	return false, fmt.Errorf("query native Control Plane service: %w (%s)", err, strings.TrimSpace(string(output)))
}

func (s TaskSchedulerService) Register(ctx context.Context) error {
	if err := s.Options.validate(); err != nil {
		return err
	}
	executor := s.Executor
	if executor == nil {
		executor = OSTaskSchedulerExecutor{}
	}
	createTemp := s.CreateTemp
	if createTemp == nil {
		createTemp = os.CreateTemp
	}
	remove := s.Remove
	if remove == nil {
		remove = os.Remove
	}
	temporary, err := createTemp("", "agent-workflow-control-plane-*.xml")
	if err != nil {
		return fmt.Errorf("create Task Scheduler definition: %w", err)
	}
	temporaryPath := temporary.Name()
	defer remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.WriteString(taskSchedulerXML(s.Options)); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	output, err := executor.Run(ctx, "schtasks.exe", "/create", "/tn", NativeServiceName, "/xml", temporaryPath)
	if err != nil {
		return fmt.Errorf("register native Control Plane service: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func taskSchedulerXML(options NativeServiceOptions) string {
	arguments := `watch-service --workflow-home "` + strings.ReplaceAll(options.WorkflowHome, `"`, `\"`) + `"`
	return `<?xml version="1.0" encoding="UTF-8"?>
<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo><Description>Agent Workflow native Control Plane</Description></RegistrationInfo>
  <Triggers><RegistrationTrigger><Enabled>true</Enabled></RegistrationTrigger></Triggers>
  <Principals><Principal id="Author"><RunLevel>LeastPrivilege</RunLevel></Principal></Principals>
  <Settings><MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy><StartWhenAvailable>true</StartWhenAvailable><AllowStartOnDemand>true</AllowStartOnDemand></Settings>
  <Actions Context="Author"><Exec><Command>` + xmlEscape(options.Executable) + `</Command><Arguments>` + xmlEscape(arguments) + `</Arguments></Exec></Actions>
</Task>`
}

var _ NativeService = TaskSchedulerService{}
