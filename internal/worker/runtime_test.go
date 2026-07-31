package worker

import (
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
}

func TestDockerArgsIncludeAuditedGatewayHostMapping(t *testing.T) {
	spec := Spec{
		Command: []string{"codex", "exec"}, WorkspacePath: "workspace", CodexStatePath: "state", Branch: "ticket-1",
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
}

func TestContainerPathMapsHostMounts(t *testing.T) {
	host := filepath.Join(t.TempDir(), "codex")
	mounts := []Mount{{Source: host, Target: "/codex-state"}}
	got := containerPath(filepath.Join(host, "output-schema.json"), mounts)
	if got != "/codex-state/output-schema.json" {
		t.Fatalf("container path = %q", got)
	}
}
