package github

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspacePusherUsesTemporaryCredentialStore(t *testing.T) {
	workspace := t.TempDir()
	pusher := WorkspacePusher{WorkspacePath: workspace, Token: "secret-token"}
	storePath, err := pusher.createCredentialStore("https://github.com/owner/repo.git")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(storePath)
	if filepath.Dir(storePath) != workspace {
		t.Fatalf("credential store directory = %q, want workspace %q", filepath.Dir(storePath), workspace)
	}
	contents, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), "x-access-token:secret-token@github.com") {
		t.Fatalf("credential store = %q", contents)
	}
	cmd := pushCommand(context.Background(), "git", workspace, storePath, "refs/heads/ticket-1:", "https://github.com/owner/repo.git", "abc123", "ticket-1")
	for _, argument := range cmd.Args {
		if strings.Contains(argument, "secret-token") {
			t.Fatalf("credential leaked into git arguments: %#v", cmd.Args)
		}
	}
}

func TestWorkspacePusherRejectsNonHTTPSCredentialStore(t *testing.T) {
	_, err := (WorkspacePusher{WorkspacePath: t.TempDir(), Token: "secret-token"}).createCredentialStore("git@github.com:owner/repo.git")
	if err == nil {
		t.Fatal("non-HTTPS push URL was accepted")
	}
}
