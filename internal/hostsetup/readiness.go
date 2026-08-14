package hostsetup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type CommandExecutor interface {
	Run(context.Context, []string) ([]byte, error)
}
type OSCommandExecutor struct{}

func (OSCommandExecutor) Run(ctx context.Context, args []string) ([]byte, error) {
	if len(args) == 0 {
		return nil, errors.New("command is empty")
	}
	return exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
}

// VerifyDockerWorker proves the actual selected state/workspace paths can be
// mounted into the immutable Worker and that the container has DNS/network
// connectivity. The temporary mount markers are always removed.
func VerifyDockerWorker(ctx context.Context, executor CommandExecutor, image, stateRoot, workspaceRoot string) error {
	if executor == nil {
		executor = OSCommandExecutor{}
	}
	if image == "" || !filepath.IsAbs(stateRoot) || !filepath.IsAbs(workspaceRoot) {
		return errors.New("Docker readiness requires an immutable image and absolute state/workspace roots")
	}
	info, err := executor.Run(ctx, []string{"docker", "info", "--format", "{{.OSType}}/{{.Architecture}}"})
	engine := strings.TrimSpace(string(info))
	if err != nil || engine != "linux/x86_64" && engine != "linux/amd64" {
		return fmt.Errorf("Docker Engine must be Linux amd64: %w (%s)", err, engine)
	}
	stateProbe, err := os.MkdirTemp(stateRoot, ".setup-readiness-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stateProbe)
	workspaceProbe, err := os.MkdirTemp(workspaceRoot, ".setup-readiness-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(workspaceProbe)
	marker := []byte("agent-workflow-readiness\n")
	if err := os.WriteFile(filepath.Join(stateProbe, "marker"), marker, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(workspaceProbe, "marker"), marker, 0o600); err != nil {
		return err
	}
	args := []string{"docker", "run", "--rm", "--network", "bridge",
		"--mount", "type=bind,source=" + stateProbe + ",target=/workflow-state,readonly",
		"--mount", "type=bind,source=" + workspaceProbe + ",target=/workspace,readonly",
		image, "sh", "-lc", "test \"$(cat /workflow-state/marker)\" = agent-workflow-readiness && test \"$(cat /workspace/marker)\" = agent-workflow-readiness && getent hosts github.com >/dev/null"}
	output, err := executor.Run(ctx, args)
	if err != nil {
		return fmt.Errorf("Docker Worker mount/connectivity probe: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}
