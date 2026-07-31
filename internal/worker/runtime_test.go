package worker

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestSpecRejectsGitHubWriteCredentialsAndRequiresAuditInputs(t *testing.T) {
	spec := Spec{Command: []string{"codex", "exec"}, WorkspacePath: "workspace", CodexStatePath: "state", Branch: "ticket-1", AgentIdentity: "agent-1", ImageDigest: "sha256:image", ToolVersions: map[string]string{"codex": "1.0"}, Environment: map[string]string{"GITHUB_TOKEN": "secret"}}
	if !errors.Is(spec.Validate(), ErrGitHubCredential) {
		t.Fatalf("Validate error = %v, want ErrGitHubCredential", spec.Validate())
	}
	delete(spec.Environment, "GITHUB_TOKEN")
	spec.ToolVersions = nil
	if spec.Validate() == nil {
		t.Fatal("Validate accepted a spec without tool versions")
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
