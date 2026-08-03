package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

var ErrGitHubCredential = errors.New("worker spec contains a GitHub write credential")

const GatewayHostMapping = "host.docker.internal:host-gateway"

type Mount struct {
	Source   string
	Target   string
	ReadOnly bool
}

type Spec struct {
	RunID          string
	Command        []string
	WorkspacePath  string
	CodexStatePath string
	Branch         string
	AgentIdentity  string
	ImageDigest    string
	ToolVersions   map[string]string
	Environment    map[string]string
	Mounts         []Mount
	ExtraHosts     []string
}

type Result struct {
	Output      []byte
	Stdout      []byte
	Stderr      []byte
	ContainerID string
	ExitCode    int
}

type Runtime interface {
	Run(context.Context, Spec) (Result, error)
}

type ContainerInspector interface {
	ContainerRunning(context.Context, string) (bool, error)
}

func (s Spec) Validate() error {
	if len(s.Command) == 0 || s.WorkspacePath == "" || s.CodexStatePath == "" || s.Branch == "" || s.AgentIdentity == "" {
		return errors.New("worker spec is incomplete")
	}
	if s.ImageDigest == "" {
		return errors.New("worker image digest is required")
	}
	if len(s.ToolVersions) == 0 {
		return errors.New("worker tool versions are required")
	}
	if !contains(s.ExtraHosts, GatewayHostMapping) {
		return errors.New("worker Gateway host mapping is required")
	}
	for name := range s.Environment {
		if IsGitHubCredentialName(name) {
			return fmt.Errorf("%w: %s", ErrGitHubCredential, name)
		}
	}
	return nil
}

// IsGitHubCredentialName is the single production classifier used to keep
// GitHub write credentials outside Worker processes and containers.
func IsGitHubCredentialName(name string) bool {
	name = strings.ToUpper(name)
	return name == "GH_TOKEN" ||
		name == "GITHUB_TOKEN" ||
		name == "GH_ENTERPRISE_TOKEN" ||
		name == "GITHUB_ENTERPRISE_TOKEN" ||
		name == "GH_PAT" ||
		name == "GITHUB_PAT" ||
		name == "GH_OAUTH_TOKEN" ||
		name == "GITHUB_OAUTH_TOKEN" ||
		strings.Contains(name, "GITHUB_TOKEN") ||
		strings.Contains(name, "GITHUB_PAT") ||
		strings.Contains(name, "GITHUB_OAUTH")
}

// ProcessRuntime is the host-process adapter used by local development and
// tests. Production container adapters implement Runtime and receive the same
// fully-audited Spec. It intentionally passes only the explicit environment.
type ProcessRuntime struct {
	Binary string
}

// DockerRuntime is the replaceable Worker container adapter. The workspace
// and Codex state mounts are host-owned, so --rm only destroys compute after a
// run and never the Ticket Session's durable state.
type DockerRuntime struct {
	Binary string
}

func (r DockerRuntime) Run(ctx context.Context, spec Spec) (Result, error) {
	if err := spec.Validate(); err != nil {
		return Result{}, err
	}
	name := r.Binary
	if name == "" {
		name = "docker"
	}
	cidfile, err := os.CreateTemp("", "workflow-worker-cid-*")
	if err != nil {
		return Result{}, fmt.Errorf("create worker container id file: %w", err)
	}
	cidfilePath := cidfile.Name()
	if err := cidfile.Close(); err != nil {
		_ = os.Remove(cidfilePath)
		return Result{}, fmt.Errorf("close worker container id file: %w", err)
	}
	if err := os.Remove(cidfilePath); err != nil {
		return Result{}, fmt.Errorf("prepare worker container id file: %w", err)
	}
	defer os.Remove(cidfilePath)
	args := dockerArgs(spec)
	args = append(args[:2], append([]string{"--cidfile", cidfilePath}, args[2:]...)...)
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	output := append(append([]byte(nil), stdout.Bytes()...), stderr.Bytes()...)
	containerID, readErr := os.ReadFile(cidfilePath)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return Result{Output: output}, fmt.Errorf("read worker container id: %w", readErr)
	}
	result := Result{Output: output, Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ContainerID: strings.TrimSpace(string(containerID))}
	if err != nil {
		result.ExitCode = 1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			result.ExitCode = exitErr.ExitCode()
		}
	}
	return result, err
}

func dockerArgs(spec Spec) []string {
	args := []string{"run", "--rm", "--workdir", "/workspace"}
	if spec.RunID != "" {
		args = append(args, "--label", "workflow.run_id="+spec.RunID)
	}
	for _, host := range spec.ExtraHosts {
		args = append(args, "--add-host", host)
	}
	for _, mount := range spec.Mounts {
		value := "type=bind,source=" + mount.Source + ",target=" + mount.Target
		if mount.ReadOnly {
			value += ",readonly"
		}
		args = append(args, "--mount", value)
	}
	keys := make([]string, 0, len(spec.Environment))
	for key := range spec.Environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if !IsGitHubCredentialName(key) {
			value := spec.Environment[key]
			if key == "CODEX_HOME" {
				value = containerPath(value, spec.Mounts)
			}
			args = append(args, "--env", key+"="+value)
		}
	}
	args = append(args, spec.ImageDigest)
	for _, value := range spec.Command {
		args = append(args, containerPath(value, spec.Mounts))
	}
	return args
}

func (r DockerRuntime) ContainerRunning(ctx context.Context, runID string) (bool, error) {
	if strings.TrimSpace(runID) == "" {
		return false, errors.New("worker run ID is required")
	}
	name := r.Binary
	if name == "" {
		name = "docker"
	}
	output, err := exec.CommandContext(ctx, name, "container", "ls", "--quiet", "--filter", "label=workflow.run_id="+runID).Output()
	if err != nil {
		return false, fmt.Errorf("inspect worker container: %w", err)
	}
	return strings.TrimSpace(string(output)) != "", nil
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func containerPath(value string, mounts []Mount) string {
	for _, mount := range mounts {
		if value == mount.Source {
			return mount.Target
		}
		prefix := strings.TrimRight(mount.Source, `/\\`) + string(os.PathSeparator)
		if strings.HasPrefix(value, prefix) {
			return strings.TrimRight(mount.Target, "/") + "/" + strings.TrimPrefix(value, prefix)
		}
	}
	return value
}

func (r ProcessRuntime) Run(ctx context.Context, spec Spec) (Result, error) {
	if err := spec.Validate(); err != nil {
		return Result{}, err
	}
	if len(spec.Command) == 0 {
		return Result{}, errors.New("worker command is empty")
	}
	name := spec.Command[0]
	if r.Binary != "" {
		name = r.Binary
	}
	cmd := exec.CommandContext(ctx, name, spec.Command[1:]...)
	cmd.Dir = spec.WorkspacePath
	cmd.Env = explicitEnvironment(spec.Environment)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	output := append(append([]byte(nil), stdout.Bytes()...), stderr.Bytes()...)
	result := Result{Output: output, Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: 0}
	if err != nil {
		result.ExitCode = 1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			result.ExitCode = exitErr.ExitCode()
		}
	}
	return result, err
}

func explicitEnvironment(values map[string]string) []string {
	result := make([]string, 0, len(values)+1)
	for name, value := range values {
		if IsGitHubCredentialName(name) {
			continue
		}
		result = append(result, name+"="+value)
	}
	if _, ok := values["PATH"]; !ok {
		result = append(result, "PATH="+os.Getenv("PATH"))
	}
	sort.Strings(result)
	return result
}
