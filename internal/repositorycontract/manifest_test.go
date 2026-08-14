package repositorycontract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestRenderMultiContextChangesOnlyDeclaredDomainConfiguration(t *testing.T) {
	files, manifest, _, err := Render("multi-context", nil, "owner/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	if manifest.DomainLayout != "multi-context" || !strings.Contains(string(files["docs/agents/domain.md"]), "CONTEXT-MAP.md") {
		t.Fatalf("manifest=%#v domain=%q", manifest, files["docs/agents/domain.md"])
	}
}
