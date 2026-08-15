package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/skyhuang233/workflow/internal/store"
	"github.com/skyhuang233/workflow/internal/workflowhome"
)

func TestRepositoryRunnerComposesGatewayAndBusinessLoopWithoutPAT(t *testing.T) {
	layout, err := workflowhome.Resolve(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	pat := "ghp_must_never_be_in_process_arguments"
	if err := os.WriteFile(layout.CredentialFile, []byte(pat), 0o600); err != nil {
		t.Fatal(err)
	}
	config := store.RepositoryRuntimeConfiguration{
		Repository: "owner/repo", DefaultBranch: "main", SourcePath: filepath.Join(t.TempDir(), "repo"), RootIssueNumber: 42,
		WorkspaceRoot: filepath.Join(layout.Workspaces, "owner-repo"), StateRoot: filepath.Join(layout.State, "codex", "owner-repo"), CodexAuthFile: filepath.Join(t.TempDir(), "auth.json"),
		GitHubAPIURL: "https://api.github.com", PollInterval: 45 * time.Second, WorkspaceRetention: 48 * time.Hour, MaxParallelRuns: 2, UpdatedAt: time.Now().UTC(),
	}
	const controlToken = "ephemeral-control-token"
	gateway, poll := (commandRepositoryRunner{Layout: layout, Owner: "owner"}).processArguments(config, 18787, controlToken)
	joinedGateway, joinedPoll := strings.Join(gateway, "\x00"), strings.Join(poll, "\x00")
	for _, expected := range []string{"gateway", filepath.Join(layout.State, "workflow.db"), "0.0.0.0:18787", controlToken, `state\credentials\github.pat`} {
		if !strings.Contains(joinedGateway, expected) {
			t.Fatalf("Gateway arguments lack %q: %q", expected, gateway)
		}
	}
	for _, expected := range []string{"poll-github", config.Repository, strconv.FormatInt(config.RootIssueNumber, 10), config.SourcePath, config.WorkspaceRoot, config.StateRoot, config.CodexAuthFile, "http://host.docker.internal:18787", "http://127.0.0.1:18787", config.PollInterval.String()} {
		if !strings.Contains(joinedPoll, expected) {
			t.Fatalf("business-loop arguments lack %q: %q", expected, poll)
		}
	}
	if strings.Contains(joinedGateway, pat) || strings.Contains(joinedPoll, pat) {
		t.Fatal("repository runtime arguments exposed the PAT")
	}
}
