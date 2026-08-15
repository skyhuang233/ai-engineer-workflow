package hostsetup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type CommandExecutor interface {
	Run(context.Context, []string) ([]byte, error)
}
type OSCommandExecutor struct{}

type DockerWorkerVerifier struct {
	Executor  CommandExecutor
	RemoveAll func(string) error
}

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
	return (DockerWorkerVerifier{Executor: executor}).Verify(ctx, image, stateRoot, workspaceRoot)
}

func (v DockerWorkerVerifier) Verify(ctx context.Context, image, stateRoot, workspaceRoot string) (resultErr error) {
	executor := v.Executor
	if executor == nil {
		executor = OSCommandExecutor{}
	}
	removeAll := v.RemoveAll
	if removeAll == nil {
		removeAll = os.RemoveAll
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
	defer func() {
		if cleanupErr := removeAll(stateProbe); cleanupErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("remove Docker state readiness probe %q: %w", stateProbe, cleanupErr))
		}
	}()
	workspaceProbe, err := os.MkdirTemp(workspaceRoot, ".setup-readiness-")
	if err != nil {
		return err
	}
	defer func() {
		if cleanupErr := removeAll(workspaceProbe); cleanupErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("remove Docker workspace readiness probe %q: %w", workspaceProbe, cleanupErr))
		}
	}()
	marker := []byte("agent-workflow-readiness\n")
	if err := os.WriteFile(filepath.Join(stateProbe, "marker"), marker, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(workspaceProbe, "marker"), marker, 0o600); err != nil {
		return err
	}
	tokenBytes := make([]byte, 24)
	if _, err := rand.Read(tokenBytes); err != nil {
		return err
	}
	token := hex.EncodeToString(tokenBytes)
	containerBytes := make([]byte, 12)
	if _, err := rand.Read(containerBytes); err != nil {
		return err
	}
	containerName := "workflow-setup-docker-" + hex.EncodeToString(containerBytes)
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		output, cleanupErr := executor.Run(cleanupCtx, []string{"docker", "rm", "-f", containerName})
		if cleanupErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("remove Docker readiness container %q: %w (%s)", containerName, cleanupErr, strings.TrimSpace(string(output))))
		}
	}()
	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		return fmt.Errorf("listen for Docker Gateway readiness probe: %w", err)
	}
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte("gateway=ok"))
	})}
	go func() { _ = server.Serve(listener) }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if cleanupErr := server.Shutdown(shutdownCtx); cleanupErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("stop Docker Gateway readiness probe: %w", cleanupErr))
		}
	}()
	port := listener.Addr().(*net.TCPAddr).Port
	script := `set -eu
test "$(cat /workflow-state/marker)" = agent-workflow-readiness
test "$(cat /workspace/marker)" = agent-workflow-readiness
printf 'worker\n' > /workflow-state/container-marker
printf 'worker\n' > /workspace/container-marker
curl --fail --silent --show-error -H "Authorization: Bearer ${WORKFLOW_GATEWAY_PROBE_TOKEN}" "http://host.docker.internal:${WORKFLOW_GATEWAY_PROBE_PORT}/health"`
	args := []string{"docker", "run", "--name", containerName, "--label", "com.skyhuang233.workflow.setup-probe=true", "--network", "bridge", "--add-host", "host.docker.internal:host-gateway",
		"--mount", "type=bind,source=" + stateProbe + ",target=/workflow-state",
		"--mount", "type=bind,source=" + workspaceProbe + ",target=/workspace",
		"--env", "WORKFLOW_GATEWAY_PROBE_TOKEN=" + token,
		"--env", fmt.Sprintf("WORKFLOW_GATEWAY_PROBE_PORT=%d", port),
		image, "sh", "-lc", script}
	output, err := executor.Run(ctx, args)
	if err != nil {
		return fmt.Errorf("Docker Worker mount/connectivity probe: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	for name, root := range map[string]string{"state": stateProbe, "workspace": workspaceProbe} {
		marker, readErr := os.ReadFile(filepath.Join(root, "container-marker"))
		if readErr != nil || string(marker) != "worker\n" {
			return errors.Join(fmt.Errorf("Docker Worker %s mount is not writable", name), readErr)
		}
	}
	return nil
}
