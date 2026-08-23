package repositorycontract

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRenderAndVerifyPreservesAgentContentOutsideManagedBlock(t *testing.T) {
	root := t.TempDir()
	existing := []byte("# Existing\n\nKeep this.\n")
	rendered, manifest, digest, err := Render("", existing, "owner/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := Write(root, rendered); err != nil {
		t.Fatal(err)
	}
	if digest == "" || manifest.Repository != "owner/repo" {
		t.Fatalf("manifest=%#v digest=%q", manifest, digest)
	}
	agents, err := os.ReadFile(filepath.Join(root, "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(agents[:len(existing)]) != string(existing) {
		t.Fatal("existing AGENTS.md bytes not preserved")
	}
	verified, err := Verify(root)
	if err != nil {
		t.Fatal(err)
	}
	if verified != digest {
		t.Fatalf("verified=%q want=%q", verified, digest)
	}
	if err := os.WriteFile(filepath.Join(root, "docs", "agents", "domain.md"), []byte("drift"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(root); err == nil {
		t.Fatal("managed file drift accepted")
	}
}

func TestRenderedWorkflowQuotesDefaultBranchAsOneYAMLScalar(t *testing.T) {
	for _, branch := range []string{"release,#1", "feature/{alpha,beta}", "true"} {
		t.Run(branch, func(t *testing.T) {
			files, _, _, err := Render("single-context", nil, "owner/repo", branch)
			if err != nil {
				t.Fatal(err)
			}
			var document yaml.Node
			if err := yaml.Unmarshal(files[".github/workflows/workflow-contract.yml"], &document); err != nil {
				t.Fatalf("rendered workflow is invalid YAML: %v\n%s", err, files[".github/workflows/workflow-contract.yml"])
			}
			var decoded map[string]any
			if err := yaml.Unmarshal(files[".github/workflows/workflow-contract.yml"], &decoded); err != nil {
				t.Fatal(err)
			}
			on, ok := decoded["on"].(map[string]any)
			if !ok {
				t.Fatalf("workflow on node = %#v", decoded["on"])
			}
			push, ok := on["push"].(map[string]any)
			if !ok {
				t.Fatalf("workflow push node = %#v", on["push"])
			}
			branches, ok := push["branches"].([]any)
			if !ok || len(branches) != 1 || branches[0] != branch {
				t.Fatalf("workflow branches = %#v, want one scalar %q", push["branches"], branch)
			}
		})
	}
}

func TestVerifyRemoteAllowsUserOwnedAgentsBytesButRejectsManagedDrift(t *testing.T) {
	files, _, digest, err := Render("", []byte("# User instructions\n\n"), "owner/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	files["AGENTS.md"] = append(files["AGENTS.md"], []byte("\nUser-owned suffix.\n")...)
	if _, err := VerifyRemote(func(path string) ([]byte, error) { return files[path], nil }, "owner/repo", "main", digest); err != nil {
		t.Fatal(err)
	}
	files["docs/agents/domain.md"] = []byte("drift")
	if _, err := VerifyRemote(func(path string) ([]byte, error) { return files[path], nil }, "owner/repo", "main", digest); err == nil {
		t.Fatal("accepted managed remote drift")
	}
}

func TestVerifyRemoteRejectsDuplicateManagedBlockMarkers(t *testing.T) {
	files, _, digest, err := Render("", nil, "owner/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	files["AGENTS.md"] = append(files["AGENTS.md"], []byte(BlockStart+"\n")...)
	if _, err := VerifyRemote(func(path string) ([]byte, error) { return files[path], nil }, "owner/repo", "main", digest); err == nil {
		t.Fatal("duplicate managed block marker accepted")
	}
}

func TestRenderMultiContextChangesOnlyDeclaredDomainConfiguration(t *testing.T) {
	files, manifest, _, err := Render("multi-context", nil, "owner/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	if manifest.DomainLayout != "multi-context" || !strings.Contains(string(files["docs/agents/domain.md"]), "CONTEXT-MAP.md") {
		t.Fatalf("manifest=%#v domain=%q", manifest, files["docs/agents/domain.md"])
	}
}

func TestRenderedAgentsBlockMatchesPublishedRepositoryContractTemplate(t *testing.T) {
	template, err := os.ReadFile(filepath.Join("..", "..", "deploy", "platform", "repository-contract", "AGENTS.block.md"))
	if err != nil {
		t.Fatal(err)
	}
	files, _, _, err := Render("single-context", nil, "owner/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	managed, ok := extractBlock(files["AGENTS.md"])
	if !ok {
		t.Fatal("rendered AGENTS.md has no managed block")
	}
	template = bytes.ReplaceAll(template, []byte("\r\n"), []byte("\n"))
	if string(managed) != string(template) {
		t.Fatalf("rendered managed block diverged from release template:\n--- rendered\n%s--- template\n%s", managed, template)
	}
	if !strings.Contains(string(managed), "`$agent-workflow`") {
		t.Fatal("rendered managed block does not automatically load the Workflow Skill Bundle")
	}
}

func TestRenderedWorkflowVerifiesExactManagedAgentsBlockDigest(t *testing.T) {
	files, _, _, err := Render("single-context", nil, "owner/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(files[".github/workflows/workflow-contract.yml"])
	for _, required := range []string{"managed_block_sha256", "hashlib.sha256", "data.count(start_marker) != 1", "agent-workflow:start", "agent-workflow:end"} {
		if !strings.Contains(workflow, required) {
			t.Fatalf("workflow does not verify exact AGENTS block digest: missing %q", required)
		}
	}
}

func TestRuntimeContractTemplatesAreByteIdenticalToDeployInputs(t *testing.T) {
	files, manifest, _, err := Render("single-context", nil, "owner/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	deployRoot := filepath.Join("..", "..", "deploy", "platform", "repository-contract")
	for _, path := range []string{"AGENTS.md", "docs/agents/issue-tracker.md", "docs/agents/domain.md", ".github/workflows/workflow-contract.yml"} {
		deployPath := path
		if path == "AGENTS.md" {
			deployPath = "AGENTS.block.md"
		}
		expected, err := os.ReadFile(filepath.Join(deployRoot, filepath.FromSlash(deployPath)))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(files[path], bytes.ReplaceAll(expected, []byte("\r\n"), []byte("\n"))) {
			t.Fatalf("runtime contract surface %s differs from deploy template", path)
		}
	}
	blockBytes, err := os.ReadFile(filepath.Join(deployRoot, "AGENTS.block.md"))
	if err != nil {
		t.Fatal(err)
	}
	blockBytes = bytes.ReplaceAll(blockBytes, []byte("\r\n"), []byte("\n"))
	sum := sha256.Sum256(blockBytes)
	if manifest.ManagedBlockSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("managed block digest parity = %q", manifest.ManagedBlockSHA256)
	}
}

func TestGeneratedRuntimeContractTemplatesAreCurrentWithDeployCanonicalSource(t *testing.T) {
	command := exec.Command("go", "run", "./cmd/gentemplates", "-check", "-source", filepath.Join("..", "..", "deploy", "platform", "repository-contract"), "-output", "templates_generated.go")
	command.Dir = "."
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generated Repository Contract templates are stale: %v\n%s", err, output)
	}
}
