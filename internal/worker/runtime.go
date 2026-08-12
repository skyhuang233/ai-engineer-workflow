package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/skyhuang233/workflow/internal/workerrun"
)

var ErrGitHubCredential = errors.New("worker spec contains a GitHub write credential")

// InfrastructureError marks failures to start or communicate with the Worker
// runtime. It lets the Control Plane back off without treating an unavailable
// host as a Ticket Agent implementation failure.
type InfrastructureError struct{ Err error }

func (e InfrastructureError) Error() string               { return e.Err.Error() }
func (e InfrastructureError) Unwrap() error               { return e.Err }
func (e InfrastructureError) InfrastructureFailure() bool { return true }

func IsInfrastructureFailure(err error) bool {
	var failure interface{ InfrastructureFailure() bool }
	return errors.As(err, &failure) && failure.InfrastructureFailure()
}

type CertifiedNoLaunchError struct{ Err error }

func (e CertifiedNoLaunchError) Error() string               { return e.Err.Error() }
func (e CertifiedNoLaunchError) Unwrap() error               { return e.Err }
func (e CertifiedNoLaunchError) InfrastructureFailure() bool { return true }
func (e CertifiedNoLaunchError) NoContainerStarted() bool    { return true }

func IsCertifiedNoLaunchFailure(err error) bool {
	var failure interface{ NoContainerStarted() bool }
	return errors.As(err, &failure) && failure.NoContainerStarted()
}

type PreparedContainerCleanupError struct{ Err error }

func (e PreparedContainerCleanupError) Error() string               { return e.Err.Error() }
func (e PreparedContainerCleanupError) Unwrap() error               { return e.Err }
func (e PreparedContainerCleanupError) InfrastructureFailure() bool { return true }

func IsPreparedContainerCleanupFailure(err error) bool {
	var failure PreparedContainerCleanupError
	return errors.As(err, &failure)
}

type UncertainContainerStateError struct{ Err error }

func (e UncertainContainerStateError) Error() string               { return e.Err.Error() }
func (e UncertainContainerStateError) Unwrap() error               { return e.Err }
func (e UncertainContainerStateError) InfrastructureFailure() bool { return true }

func IsUncertainContainerStateFailure(err error) bool {
	var failure UncertainContainerStateError
	return errors.As(err, &failure)
}

const (
	GatewayHostMapping              = "host.docker.internal:host-gateway"
	preparedContainerCleanupTimeout = 10 * time.Second
)

// CodexSandboxDockerArgs returns the Docker permissions required by Codex's
// nested bubblewrap sandbox. Both SYS_ADMIN and an unconfined seccomp profile
// are required: the former permits namespace setup and the latter pivot_root.
func CodexSandboxDockerArgs() []string {
	return []string{"--cap-add", "SYS_ADMIN", "--security-opt", "seccomp=unconfined"}
}

type Mount struct {
	Source   string
	Target   string
	ReadOnly bool
}

type Spec struct {
	RunID                string
	RunKind              workerrun.Kind
	ControlPlaneID       string
	Command              []string
	WorkspacePath        string
	CodexStatePath       string
	Branch               string
	AgentIdentity        string
	ImageDigest          string
	ToolVersions         map[string]string
	Environment          map[string]string
	Mounts               []Mount
	ExtraHosts           []string
	ContainerPreflight   string
	ContainerCreateFence func(context.Context) (func(context.Context) error, error)
	StartAdmission       func(context.Context) error
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

// ContainerIsolator stops every container belonging to an expired Worker Run.
// It is deliberately distinct from inspection: recovery must revoke a stale
// Run's compute before dispatching a replacement generation.
type ContainerIsolator interface {
	IsolateContainer(context.Context, string) error
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
	Binary                 string
	ControlPlaneID         string
	DiskPath               string
	MemoryThresholdPercent float64
	DiskThresholdPercent   float64
}

func (r DockerRuntime) Unsafe(ctx context.Context) (bool, error) {
	reason, err := r.Inspect(ctx)
	return reason != "", err
}

// Inspect is a dispatch-only host safety gate. Any unavailable Docker health
// probe is unsafe because the controller cannot verify that it can launch a
// new Worker Run; it never changes existing containers.
func (r DockerRuntime) Inspect(ctx context.Context) (string, error) {
	binary := r.Binary
	if binary == "" {
		binary = "docker"
	}
	if _, err := exec.CommandContext(ctx, binary, "info", "--format", "{{.ServerVersion}}").Output(); err != nil {
		return "Docker health check failed", nil
	}
	memoryUsage, err := hostMemoryUsage(ctx)
	if err != nil {
		return "host memory pressure could not be inspected", nil
	}
	if memoryUsage >= r.memoryThreshold() {
		return fmt.Sprintf("host memory usage %.1f%% reached the %.0f%% threshold", memoryUsage, r.memoryThreshold()), nil
	}
	diskUsage, err := hostDiskUsage(r.DiskPath)
	if err != nil {
		return "host disk pressure could not be inspected", nil
	}
	if diskUsage >= r.diskThreshold() {
		return fmt.Sprintf("host disk usage %.1f%% reached the %.0f%% threshold", diskUsage, r.diskThreshold()), nil
	}
	return "", nil
}

func (r DockerRuntime) memoryThreshold() float64 {
	if r.MemoryThresholdPercent > 0 {
		return r.MemoryThresholdPercent
	}
	return 85
}

func (r DockerRuntime) diskThreshold() float64 {
	if r.DiskThresholdPercent > 0 {
		return r.DiskThresholdPercent
	}
	return 90
}

func (r DockerRuntime) Run(ctx context.Context, spec Spec) (Result, error) {
	if spec.ControlPlaneID == "" {
		spec.ControlPlaneID = r.ControlPlaneID
	}
	if strings.TrimSpace(string(spec.RunKind)) != "" && strings.TrimSpace(spec.ControlPlaneID) == "" {
		return Result{}, CertifiedNoLaunchError{Err: errors.New("Control Plane container identity is required for a typed Worker Run")}
	}
	if strings.TrimSpace(string(spec.RunKind)) != "" && (spec.ContainerCreateFence == nil || spec.StartAdmission == nil) {
		return Result{}, CertifiedNoLaunchError{Err: errors.New("typed Worker Run requires container create fencing and start admission")}
	}
	if err := spec.Validate(); err != nil {
		return Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return Result{}, CertifiedNoLaunchError{Err: err}
	}
	name := r.Binary
	if name == "" {
		name = "docker"
	}
	if spec.StartAdmission != nil {
		return r.runWithStartAdmission(ctx, name, spec)
	}
	cidfile, err := os.CreateTemp("", "workflow-worker-cid-*")
	if err != nil {
		return Result{}, CertifiedNoLaunchError{Err: fmt.Errorf("create worker container id file: %w", err)}
	}
	cidfilePath := cidfile.Name()
	if err := cidfile.Close(); err != nil {
		_ = os.Remove(cidfilePath)
		return Result{}, CertifiedNoLaunchError{Err: fmt.Errorf("close worker container id file: %w", err)}
	}
	if err := os.Remove(cidfilePath); err != nil {
		return Result{}, CertifiedNoLaunchError{Err: fmt.Errorf("prepare worker container id file: %w", err)}
	}
	defer os.Remove(cidfilePath)
	args := dockerArgs(spec)
	args = append(args[:2], append([]string{"--cidfile", cidfilePath}, args[2:]...)...)
	if err := ctx.Err(); err != nil {
		return Result{}, CertifiedNoLaunchError{Err: err}
	}
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	output := append(append([]byte(nil), stdout.Bytes()...), stderr.Bytes()...)
	containerID, readErr := os.ReadFile(cidfilePath)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return Result{Output: output}, InfrastructureError{Err: fmt.Errorf("read worker container id: %w", readErr)}
	}
	result := Result{Output: output, Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ContainerID: strings.TrimSpace(string(containerID))}
	if err != nil {
		result.ExitCode = 1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			result.ExitCode = exitErr.ExitCode()
			if result.ExitCode == 125 {
				return result, CertifiedNoLaunchError{Err: err}
			}
		} else {
			return result, InfrastructureError{Err: err}
		}
	}
	return result, err
}

func (r DockerRuntime) runWithStartAdmission(ctx context.Context, name string, spec Spec) (Result, error) {
	args := dockerArgs(spec)
	args[0] = "create"
	releaseCreate := func(context.Context) error { return nil }
	if spec.ContainerCreateFence != nil {
		var err error
		releaseCreate, err = spec.ContainerCreateFence(ctx)
		if err != nil {
			return Result{}, CertifiedNoLaunchError{Err: err}
		}
		if releaseCreate == nil {
			return Result{}, CertifiedNoLaunchError{Err: errors.New("worker container create fence did not provide a release function")}
		}
	}
	cmd := exec.CommandContext(ctx, name, args...)
	var createStdout, createStderr bytes.Buffer
	cmd.Stdout = &createStdout
	cmd.Stderr = &createStderr
	createErr := cmd.Run()
	releaseCtx, cancelRelease := context.WithTimeout(context.Background(), preparedContainerCleanupTimeout)
	releaseErr := releaseCreate(releaseCtx)
	cancelRelease()
	if releaseErr != nil {
		return cleanupPreparedContainers(name, spec.RunID, Result{ContainerID: strings.TrimSpace(createStdout.String())}, errors.Join(createErr, releaseErr))
	}
	if createErr != nil {
		output := append(append([]byte(nil), createStdout.Bytes()...), createStderr.Bytes()...)
		result := Result{Output: output, Stdout: createStdout.Bytes(), Stderr: createStderr.Bytes(), ContainerID: strings.TrimSpace(createStdout.String()), ExitCode: 1}
		return cleanupPreparedContainers(name, spec.RunID, result, createErr)
	}
	containerID := strings.TrimSpace(createStdout.String())
	if containerID == "" {
		cause := InfrastructureError{Err: errors.New("Docker did not report the prepared worker container ID")}
		return cleanupPreparedContainers(name, spec.RunID, Result{}, cause)
	}
	removePrepared := func() error {
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), preparedContainerCleanupTimeout)
		defer cancelCleanup()
		output, err := exec.CommandContext(cleanupCtx, name, "container", "rm", "--force", containerID).CombinedOutput()
		if err != nil {
			return fmt.Errorf("remove prepared worker container %s: %w (%s)", containerID, err, strings.TrimSpace(string(output)))
		}
		return nil
	}
	rejectPrepared := func(cause error) (Result, error) {
		if cleanupErr := removePrepared(); cleanupErr != nil {
			return Result{ContainerID: containerID}, PreparedContainerCleanupError{Err: errors.Join(cause, cleanupErr)}
		}
		return Result{}, CertifiedNoLaunchError{Err: cause}
	}
	if err := ctx.Err(); err != nil {
		return rejectPrepared(err)
	}
	if err := spec.StartAdmission(ctx); err != nil {
		return rejectPrepared(err)
	}
	cmd = exec.CommandContext(ctx, name, "container", "start", "--attach", containerID)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	output := append(append([]byte(nil), stdout.Bytes()...), stderr.Bytes()...)
	result := Result{Output: output, Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ContainerID: containerID}
	if err != nil {
		result.ExitCode = 1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			result.ExitCode = exitErr.ExitCode()
		}
		return result, UncertainContainerStateError{Err: err}
	}
	return result, nil
}

func cleanupPreparedContainers(name, runID string, result Result, cause error) (Result, error) {
	cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), preparedContainerCleanupTimeout)
	defer cancelCleanup()
	if cleanupErr := isolateContainersByRunID(cleanupCtx, name, runID); cleanupErr != nil {
		return result, PreparedContainerCleanupError{Err: errors.Join(cause, cleanupErr)}
	}
	return Result{}, CertifiedNoLaunchError{Err: cause}
}

func dockerArgs(spec Spec) []string {
	args := []string{"run", "--rm"}
	args = append(args, CodexSandboxDockerArgs()...)
	args = append(args, "--workdir", "/workspace")
	if spec.RunID != "" {
		args = append(args, "--label", "workflow.run_id="+spec.RunID)
	}
	if spec.ControlPlaneID != "" {
		args = append(args, "--label", "workflow.control_plane="+spec.ControlPlaneID)
	}
	if spec.RunKind != "" {
		args = append(args, "--label", "workflow.run_kind="+string(spec.RunKind))
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
	command := spec.Command
	if spec.ContainerPreflight != "" {
		command = append([]string{"sh", "-ceu", spec.ContainerPreflight, "--"}, command...)
	}
	for _, value := range command {
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

func (r DockerRuntime) IsolateContainer(ctx context.Context, runID string) error {
	if strings.TrimSpace(runID) == "" {
		return errors.New("worker run ID is required")
	}
	name := r.Binary
	if name == "" {
		name = "docker"
	}
	return isolateContainersByRunID(ctx, name, runID)
}

func (r DockerRuntime) IsolateControlPlaneContainers(ctx context.Context) error {
	if strings.TrimSpace(r.ControlPlaneID) == "" {
		return errors.New("Control Plane container identity is required")
	}
	name := r.Binary
	if name == "" {
		name = "docker"
	}
	output, err := exec.CommandContext(ctx, name, "container", "ls", "--all", "--quiet", "--filter", "label=workflow.run_id").Output()
	if err != nil {
		return fmt.Errorf("inspect Control Plane worker containers: %w", err)
	}
	var ambiguous []string
	for _, containerID := range strings.Fields(string(output)) {
		labelsOutput, err := exec.CommandContext(ctx, name, "container", "inspect", "--format", "{{json .Config.Labels}}", containerID).Output()
		if err != nil {
			return fmt.Errorf("inspect workflow container %s labels: %w", containerID, err)
		}
		labels := make(map[string]string)
		if err := json.Unmarshal(bytes.TrimSpace(labelsOutput), &labels); err != nil {
			return fmt.Errorf("decode workflow container %s labels: %w", containerID, err)
		}
		controlPlaneID := strings.TrimSpace(labels["workflow.control_plane"])
		if controlPlaneID == "" {
			ambiguous = append(ambiguous, containerID)
			continue
		}
	}
	if len(ambiguous) > 0 {
		sort.Strings(ambiguous)
		return fmt.Errorf("ambiguous legacy workflow containers require manual isolation: %s", strings.Join(ambiguous, ", "))
	}
	return isolateContainersByLabels(ctx, name, "label=workflow.control_plane="+r.ControlPlaneID)
}

func isolateContainersByRunID(ctx context.Context, name, runID string) error {
	return isolateContainersByLabels(ctx, name, "label=workflow.run_id="+runID)
}

func isolateContainersByLabels(ctx context.Context, name string, filters ...string) error {
	args := []string{"container", "ls", "--all", "--quiet"}
	for _, filter := range filters {
		args = append(args, "--filter", filter)
	}
	output, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return fmt.Errorf("inspect expired worker container: %w", err)
	}
	return removeContainers(ctx, name, strings.Fields(string(output)))
}

func removeContainers(ctx context.Context, name string, containerIDs []string) error {
	for _, containerID := range containerIDs {
		if err := exec.CommandContext(ctx, name, "container", "rm", "--force", containerID).Run(); err != nil {
			return fmt.Errorf("isolate expired worker container %s: %w", containerID, err)
		}
	}
	return nil
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
	if err := ctx.Err(); err != nil {
		return Result{}, CertifiedNoLaunchError{Err: err}
	}
	if spec.StartAdmission != nil {
		if err := spec.StartAdmission(ctx); err != nil {
			return Result{}, CertifiedNoLaunchError{Err: err}
		}
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
