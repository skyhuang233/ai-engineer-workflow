//go:build windows

package controlplane

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type taskExecutorFunc func(context.Context, string, ...string) ([]byte, error)

func (f taskExecutorFunc) Run(ctx context.Context, command string, args ...string) ([]byte, error) {
	return f(ctx, command, args...)
}

func TestTaskSchedulerServiceTreatsOnlyAbsenceAsMissing(t *testing.T) {
	service := TaskSchedulerService{Options: NativeServiceOptions{Executable: `C:\Program Files\Agent Workflow\workflow.exe`, WorkflowHome: `C:\Users\me\AppData\Local\AgentWorkflow`}, Executor: taskExecutorFunc(func(context.Context, string, ...string) ([]byte, error) {
		return []byte("ERROR: The system cannot find the file specified."), errors.New("exit")
	})}
	present, err := service.Present(context.Background())
	if err != nil || present {
		t.Fatalf("present=%v err=%v", present, err)
	}
}

func TestTaskSchedulerServiceRegistersARegistrationTriggerWithoutOverwrite(t *testing.T) {
	var invocation []string
	service := TaskSchedulerService{Options: NativeServiceOptions{Executable: `C:\Program Files\Agent Workflow\workflow.exe`, WorkflowHome: `C:\Users\me\AppData\Local\AgentWorkflow`}, Executor: taskExecutorFunc(func(_ context.Context, command string, args ...string) ([]byte, error) {
		invocation = append([]string{command}, args...)
		return nil, nil
	})}
	if err := service.Register(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(invocation, " ") == "" || strings.Contains(strings.Join(invocation, " "), "/f") {
		t.Fatalf("registration=%q", invocation)
	}
	definition := taskSchedulerXML(service.Options)
	for _, required := range []string{"<RegistrationTrigger>", "<MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>", "watch-service", "workflow.exe"} {
		if !strings.Contains(definition, required) {
			t.Fatalf("definition omitted %q: %s", required, definition)
		}
	}
}

func TestTaskSchedulerXMLIsSafeForSpecialCharacters(t *testing.T) {
	definition := taskSchedulerXML(NativeServiceOptions{Executable: `C:\tool & work\workflow.exe`, WorkflowHome: `C:\Users\me & you\AgentWorkflow`})
	if strings.Contains(definition, `C:\tool & work`) || !strings.Contains(definition, `&amp;`) {
		t.Fatalf("definition=%s", definition)
	}
	if _, err := os.Stat(filepath.Dir(t.TempDir())); err != nil {
		t.Fatal(err)
	}
}
