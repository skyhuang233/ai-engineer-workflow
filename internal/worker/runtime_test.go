package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func init() {
	if os.Getenv("WORKFLOW_DOCKER_RUNTIME_HELPER") != "1" {
		return
	}
	logFile, err := os.OpenFile(os.Getenv("WORKFLOW_DOCKER_RUNTIME_LOG"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		os.Exit(2)
	}
	_, _ = fmt.Fprintln(logFile, strings.Join(os.Args[1:], " "))
	_ = logFile.Close()
	if len(os.Args) > 1 && os.Args[1] == "create" {
		_, _ = fmt.Fprintln(os.Stdout, "prepared-container")
		if os.Getenv("WORKFLOW_DOCKER_RUNTIME_CREATE_FAIL") == "1" {
			os.Exit(3)
		}
		os.Exit(0)
	}
	if len(os.Args) > 3 && os.Args[1] == "container" && os.Args[2] == "start" {
		_, _ = fmt.Fprintln(os.Stdout, "started")
		os.Exit(0)
	}
	if len(os.Args) > 3 && os.Args[1] == "container" && os.Args[2] == "ls" {
		_, _ = fmt.Fprintln(os.Stdout, "prepared-container")
		os.Exit(0)
	}
	if len(os.Args) > 4 && os.Args[1] == "container" && os.Args[2] == "inspect" {
		labels := os.Getenv("WORKFLOW_DOCKER_RUNTIME_INSPECT_LABELS")
		if labels == "" {
			labels = `{"workflow.run_id":"run-1","workflow.control_plane":"control-1","workflow.run_kind":"delivery_controller"}`
		}
		_, _ = fmt.Fprintln(os.Stdout, labels)
		os.Exit(0)
	}
	if len(os.Args) > 3 && os.Args[1] == "container" && os.Args[2] == "rm" {
		if os.Getenv("WORKFLOW_DOCKER_RUNTIME_RM_FAIL") == "1" {
			os.Exit(3)
		}
		os.Exit(0)
	}
	os.Exit(2)
}

func TestSpecRejectsGitHubWriteCredentialsAndRequiresAuditInputs(t *testing.T) {
	spec := Spec{Command: []string{"codex", "exec"}, WorkspacePath: "workspace", CodexStatePath: "state", Branch: "ticket-1", AgentIdentity: "agent-1", ImageDigest: "sha256:image", ToolVersions: map[string]string{"codex": "1.0"}, Environment: map[string]string{"GITHUB_TOKEN": "secret"}, ExtraHosts: []string{GatewayHostMapping}}
	if !errors.Is(spec.Validate(), ErrGitHubCredential) {
		t.Fatalf("Validate error = %v, want ErrGitHubCredential", spec.Validate())
	}
	delete(spec.Environment, "GITHUB_TOKEN")
	spec.ToolVersions = nil
	if spec.Validate() == nil {
		t.Fatal("Validate accepted a spec without tool versions")
	}
	t.Log("Worker spec validation rejected a GitHub credential before launch")
}

func TestDockerArgsIncludeAuditedGatewayHostMapping(t *testing.T) {
	spec := Spec{
		RunID: "run-1", Command: []string{"codex", "exec"}, WorkspacePath: "workspace", CodexStatePath: "state", Branch: "ticket-1",
		AgentIdentity: "agent-1", ImageDigest: "sha256:image", ToolVersions: map[string]string{"codex": "1.0"},
		ExtraHosts: []string{GatewayHostMapping},
	}
	args := dockerArgs(spec)
	found := false
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--add-host" && args[i+1] == GatewayHostMapping {
			found = true
		}
	}
	if !found {
		t.Fatalf("docker args omit Gateway host mapping: %#v", args)
	}
	if !containsArgs(args, "--label", "workflow.run_id=run-1") {
		t.Fatalf("docker args omit run label: %#v", args)
	}
	if !containsArgs(args, "--cap-add", "SYS_ADMIN") {
		t.Fatalf("docker args omit bubblewrap namespace capability: %#v", args)
	}
	if !containsArgs(args, "--security-opt", "seccomp=unconfined") {
		t.Fatalf("docker args omit bubblewrap-compatible seccomp policy: %#v", args)
	}
}

func TestDockerArgsLabelControlPlaneAndRunKind(t *testing.T) {
	args := dockerArgs(Spec{RunID: "run-1", RunKind: "delivery_controller", ControlPlaneID: "control-1", ImageDigest: "sha256:image"})
	if !containsArgs(args, "--label", "workflow.control_plane=control-1") || !containsArgs(args, "--label", "workflow.run_kind=delivery_controller") {
		t.Fatalf("docker args omit Control Plane delivery labels: %#v", args)
	}
}

func TestDockerArgsWrapContainerPreflightAroundCommand(t *testing.T) {
	spec := Spec{
		RunID: "run-1", Command: []string{"no-mistakes", "axi", "run"}, WorkspacePath: "workspace", CodexStatePath: "state", Branch: "ticket-1",
		AgentIdentity: "agent-1", ImageDigest: "sha256:image", ToolVersions: map[string]string{"codex": "1.0"},
		ExtraHosts: []string{GatewayHostMapping}, ContainerPreflight: "verify-source",
	}
	args := dockerArgs(spec)
	if !containsArgs(args, "-ceu", "verify-source") || !containsArgs(args, "--", "no-mistakes") {
		t.Fatalf("docker args omit isolated preflight wrapper: %#v", args)
	}
}

func TestCertifiedNoLaunchFailureIsInfrastructure(t *testing.T) {
	err := CertifiedNoLaunchError{Err: errors.New("launch preparation failed")}
	if !IsCertifiedNoLaunchFailure(err) || !IsInfrastructureFailure(err) {
		t.Fatalf("certified no-launch classification = %v", err)
	}
}

func TestDockerRuntimeCertifiesPreRuntimeContextExpiry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	spec := Spec{Command: []string{"worker"}, WorkspacePath: "workspace", CodexStatePath: "state", Branch: "ticket-1", AgentIdentity: "agent-1", ImageDigest: "sha256:image", ToolVersions: map[string]string{"codex": "1.0"}, ExtraHosts: []string{GatewayHostMapping}}
	_, err := (DockerRuntime{}).Run(ctx, spec)
	if !IsCertifiedNoLaunchFailure(err) || !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-runtime context expiry = %T %v", err, err)
	}
}

func TestDockerRuntimeCreatesContainerBeforeStartAdmission(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "docker.log")
	t.Setenv("WORKFLOW_DOCKER_RUNTIME_HELPER", "1")
	t.Setenv("WORKFLOW_DOCKER_RUNTIME_LOG", logPath)
	spec := Spec{
		RunID: "run-1", RunKind: "delivery_controller", Command: []string{"worker"}, WorkspacePath: "workspace", CodexStatePath: "state", Branch: "ticket-1",
		AgentIdentity: "agent-1", ImageDigest: "sha256:image", ToolVersions: map[string]string{"codex": "1.0"}, ExtraHosts: []string{GatewayHostMapping},
	}
	fenced := false
	released := false
	spec.ContainerCreateFence = func(context.Context) (func(context.Context) error, error) {
		if _, err := os.Stat(logPath); !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("Docker create ran before create fence: %v", err)
		}
		fenced = true
		return func(context.Context) error {
			released = true
			return nil
		}, nil
	}
	spec.StartAdmission = func(context.Context) error {
		if !fenced || !released {
			return errors.New("container create fence was not held and released around Docker create")
		}
		commands, err := os.ReadFile(logPath)
		if err != nil {
			return err
		}
		if !strings.HasPrefix(string(commands), "create ") || strings.Contains(string(commands), "container start") {
			return fmt.Errorf("start admission command order = %q", commands)
		}
		return nil
	}
	result, err := (DockerRuntime{Binary: binary, ControlPlaneID: "control-1"}).Run(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	commands, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if result.ContainerID != "prepared-container" || strings.TrimSpace(string(result.Stdout)) != "started" || !strings.Contains(string(commands), "--label workflow.control_plane=control-1 --label workflow.run_kind=delivery_controller") || !strings.Contains(string(commands), "container start --attach prepared-container") {
		t.Fatalf("admitted Docker start = result %#v, commands %q", result, commands)
	}
}

func TestDockerRuntimeRejectsTypedRunWithoutControlPlaneIdentity(t *testing.T) {
	spec := Spec{
		RunID: "run-1", RunKind: "delivery_controller", Command: []string{"worker"}, WorkspacePath: "workspace", CodexStatePath: "state", Branch: "ticket-1",
		AgentIdentity: "agent-1", ImageDigest: "sha256:image", ToolVersions: map[string]string{"codex": "1.0"}, ExtraHosts: []string{GatewayHostMapping},
	}
	_, err := (DockerRuntime{}).Run(context.Background(), spec)
	if !IsCertifiedNoLaunchFailure(err) {
		t.Fatalf("unscoped typed Worker Run = %T %v", err, err)
	}
}

func TestDockerRuntimeSweepsUncertainCreateBeforeCertification(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "docker.log")
	t.Setenv("WORKFLOW_DOCKER_RUNTIME_HELPER", "1")
	t.Setenv("WORKFLOW_DOCKER_RUNTIME_LOG", logPath)
	t.Setenv("WORKFLOW_DOCKER_RUNTIME_CREATE_FAIL", "1")
	spec := Spec{
		RunID: "run-1", Command: []string{"worker"}, WorkspacePath: "workspace", CodexStatePath: "state", Branch: "ticket-1",
		AgentIdentity: "agent-1", ImageDigest: "sha256:image", ToolVersions: map[string]string{"codex": "1.0"}, ExtraHosts: []string{GatewayHostMapping},
		StartAdmission: func(context.Context) error { return nil },
	}
	_, err = (DockerRuntime{Binary: binary}).Run(context.Background(), spec)
	if !IsCertifiedNoLaunchFailure(err) {
		t.Fatalf("uncertain Docker create = %T %v", err, err)
	}
	commands, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log := string(commands)
	if !strings.Contains(log, "container ls --all --quiet --filter label=workflow.run_id=run-1") || !strings.Contains(log, "container rm --force prepared-container") {
		t.Fatalf("uncertain create cleanup commands = %q", log)
	}
}

func TestDockerRuntimeRemovesPreparedContainerAfterRejectedAdmission(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "docker.log")
	t.Setenv("WORKFLOW_DOCKER_RUNTIME_HELPER", "1")
	t.Setenv("WORKFLOW_DOCKER_RUNTIME_LOG", logPath)
	ctx, cancel := context.WithCancel(context.Background())
	spec := Spec{
		RunID: "run-1", Command: []string{"worker"}, WorkspacePath: "workspace", CodexStatePath: "state", Branch: "ticket-1",
		AgentIdentity: "agent-1", ImageDigest: "sha256:image", ToolVersions: map[string]string{"codex": "1.0"}, ExtraHosts: []string{GatewayHostMapping},
		StartAdmission: func(context.Context) error {
			cancel()
			return errors.New("lease expired")
		},
	}
	_, err = (DockerRuntime{Binary: binary}).Run(ctx, spec)
	if !IsCertifiedNoLaunchFailure(err) {
		t.Fatalf("rejected admission = %T %v", err, err)
	}
	commands, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(commands), "container rm --force prepared-container") {
		t.Fatalf("prepared container cleanup commands = %q", commands)
	}
}

func TestDockerRuntimePreservesPreparedContainerAfterCleanupFailure(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "docker.log")
	t.Setenv("WORKFLOW_DOCKER_RUNTIME_HELPER", "1")
	t.Setenv("WORKFLOW_DOCKER_RUNTIME_LOG", logPath)
	t.Setenv("WORKFLOW_DOCKER_RUNTIME_RM_FAIL", "1")
	admissionErr := errors.New("lease expired")
	spec := Spec{
		RunID: "run-1", Command: []string{"worker"}, WorkspacePath: "workspace", CodexStatePath: "state", Branch: "ticket-1",
		AgentIdentity: "agent-1", ImageDigest: "sha256:image", ToolVersions: map[string]string{"codex": "1.0"}, ExtraHosts: []string{GatewayHostMapping},
		StartAdmission: func(context.Context) error { return admissionErr },
	}
	result, err := (DockerRuntime{Binary: binary}).Run(context.Background(), spec)
	if result.ContainerID != "prepared-container" || !IsPreparedContainerCleanupFailure(err) || IsCertifiedNoLaunchFailure(err) || !IsInfrastructureFailure(err) || !errors.Is(err, admissionErr) {
		t.Fatalf("failed prepared cleanup = result %#v, error %T %v", result, err, err)
	}
}

func TestPreparedContainerCleanupTimeoutIsBounded(t *testing.T) {
	if preparedContainerCleanupTimeout <= 0 || preparedContainerCleanupTimeout > 30*time.Second {
		t.Fatalf("prepared container cleanup timeout = %s", preparedContainerCleanupTimeout)
	}
}

func TestDockerRuntimeIsolationIncludesPreparedContainers(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "docker.log")
	t.Setenv("WORKFLOW_DOCKER_RUNTIME_HELPER", "1")
	t.Setenv("WORKFLOW_DOCKER_RUNTIME_LOG", logPath)
	if err := (DockerRuntime{Binary: binary}).IsolateContainer(context.Background(), "run-1"); err != nil {
		t.Fatal(err)
	}
	commands, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log := string(commands)
	if !strings.Contains(log, "container ls --all --quiet --filter label=workflow.run_id=run-1") || !strings.Contains(log, "container rm --force prepared-container") {
		t.Fatalf("prepared container isolation commands = %q", log)
	}
}

func TestDockerRuntimeIsolatesControlPlaneDeliveryContainers(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "docker.log")
	t.Setenv("WORKFLOW_DOCKER_RUNTIME_HELPER", "1")
	t.Setenv("WORKFLOW_DOCKER_RUNTIME_LOG", logPath)
	if err := (DockerRuntime{Binary: binary, ControlPlaneID: "control-1"}).IsolateControlPlaneDeliveryContainers(context.Background()); err != nil {
		t.Fatal(err)
	}
	commands, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log := string(commands)
	if !strings.Contains(log, "container ls --all --quiet --filter label=workflow.run_id") || !strings.Contains(log, "container inspect --format {{json .Config.Labels}} prepared-container") || !strings.Contains(log, "container rm --force prepared-container") {
		t.Fatalf("Control Plane delivery isolation commands = %q", log)
	}
}

func TestDockerRuntimeRefusesAmbiguousLegacyWorkflowContainers(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "docker.log")
	t.Setenv("WORKFLOW_DOCKER_RUNTIME_HELPER", "1")
	t.Setenv("WORKFLOW_DOCKER_RUNTIME_LOG", logPath)
	t.Setenv("WORKFLOW_DOCKER_RUNTIME_INSPECT_LABELS", `{"workflow.run_id":"legacy-run"}`)
	err = (DockerRuntime{Binary: binary, ControlPlaneID: "control-1"}).IsolateControlPlaneDeliveryContainers(context.Background())
	if err == nil || !strings.Contains(err.Error(), "ambiguous legacy workflow containers") {
		t.Fatalf("legacy workflow container isolation = %v", err)
	}
	commands, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(commands), "container rm --force") {
		t.Fatalf("ambiguous legacy container was removed without ownership proof: %q", commands)
	}
}

func containsArgs(args []string, first, second string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == first && args[i+1] == second {
			return true
		}
	}
	return false
}

func TestGitHubCredentialClassifierCoversTokenAliases(t *testing.T) {
	for _, name := range []string{
		"GH_TOKEN",
		"GITHUB_TOKEN",
		"GH_ENTERPRISE_TOKEN",
		"GITHUB_ENTERPRISE_TOKEN",
		"GH_PAT",
		"GITHUB_PAT",
		"GH_OAUTH_TOKEN",
		"GITHUB_OAUTH_TOKEN",
		"MY_GITHUB_TOKEN",
	} {
		if !IsGitHubCredentialName(name) {
			t.Errorf("IsGitHubCredentialName(%q) = false", name)
		}
	}
	if IsGitHubCredentialName("WORKFLOW_GATEWAY_PROBE_TOKEN") {
		t.Fatal("Gateway probe token was misclassified as a GitHub credential")
	}
	t.Log("Worker credential isolation recognizes GH/GitHub token, PAT, OAuth, and enterprise-token aliases while retaining the non-GitHub Gateway probe token")
}

func TestContainerPathMapsHostMounts(t *testing.T) {
	host := filepath.Join(t.TempDir(), "codex")
	mounts := []Mount{{Source: host, Target: "/codex-state"}}
	got := containerPath(filepath.Join(host, "output-schema.json"), mounts)
	if got != "/codex-state/output-schema.json" {
		t.Fatalf("container path = %q", got)
	}
}
