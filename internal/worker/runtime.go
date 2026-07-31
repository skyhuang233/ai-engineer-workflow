package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

var ErrGitHubCredential = errors.New("worker spec contains a GitHub write credential")

type Mount struct {
	Source   string
	Target   string
	ReadOnly bool
}

type Spec struct {
	Command        []string
	WorkspacePath  string
	CodexStatePath string
	Branch         string
	AgentIdentity  string
	ImageDigest    string
	ToolVersions   map[string]string
	Environment    map[string]string
	Mounts         []Mount
}

type Result struct {
	Output      []byte
	ContainerID string
	ExitCode    int
}

type Runtime interface {
	Run(context.Context, Spec) (Result, error)
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
	args := []string{"run", "--rm", "--workdir", "/workspace"}
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
	cmd := exec.CommandContext(ctx, name, args...)
	output, err := cmd.CombinedOutput()
	result := Result{Output: output}
	if err != nil {
		result.ExitCode = 1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			result.ExitCode = exitErr.ExitCode()
		}
	}
	return result, err
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
	output, err := cmd.CombinedOutput()
	result := Result{Output: output, ExitCode: 0}
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
