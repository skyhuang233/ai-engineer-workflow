package worker

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

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
