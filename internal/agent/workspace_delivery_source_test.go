package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeliverySourceRefreshesPerRevisionAndPinsRetries(t *testing.T) {
	ctx := context.Background()
	source := filepath.Join(t.TempDir(), "source")
	if err := runGit(ctx, "", "init", "-b", "main", source); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{"user.name": "Test", "user.email": "test@example.com"} {
		if err := runGit(ctx, source, "config", key, value); err != nil {
			t.Fatal(err)
		}
	}
	writeDeliverySourceCommit(t, ctx, source, "first")
	manager := WorkspaceManager{RootDir: filepath.Join(t.TempDir(), "workspaces"), CodexStateRoot: filepath.Join(t.TempDir(), "codex")}
	workspacePath, _, err := manager.sessionPaths("session-1")
	if err != nil {
		t.Fatal(err)
	}
	pinnedFirst, err := manager.ensureDeliverySource(ctx, "session-1", "revision-1", workspacePath, source)
	if err != nil {
		t.Fatal(err)
	}
	writeDeliverySourceCommit(t, ctx, source, "second")
	first, err := manager.ensure(ctx, "session-1", "revision-1", source, "ticket-1")
	if err != nil {
		t.Fatal(err)
	}
	firstMain, err := gitOutput(ctx, first.DeliverySource, "rev-parse", "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	firstWorkspaceMain, err := gitOutput(ctx, first.Path, "rev-parse", "refs/remotes/origin/main")
	if err != nil {
		t.Fatal(err)
	}
	sourceMain, err := gitOutput(ctx, source, "rev-parse", "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	if first.DeliverySource != pinnedFirst || strings.TrimSpace(firstWorkspaceMain) != strings.TrimSpace(firstMain) || strings.TrimSpace(firstWorkspaceMain) == strings.TrimSpace(sourceMain) {
		t.Fatalf("workspace did not use pinned revision source: snapshot=%q workspace=%q source=%q", firstMain, firstWorkspaceMain, sourceMain)
	}
	second, err := manager.ensure(ctx, "session-1", "revision-2", source, "ticket-1")
	if err != nil {
		t.Fatal(err)
	}
	secondMain, err := gitOutput(ctx, second.DeliverySource, "rev-parse", "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	workspaceMain, err := gitOutput(ctx, second.Path, "rev-parse", "refs/remotes/origin/main")
	if err != nil {
		t.Fatal(err)
	}
	if first.DeliverySource == second.DeliverySource || strings.TrimSpace(firstMain) == strings.TrimSpace(secondMain) || strings.TrimSpace(workspaceMain) != strings.TrimSpace(secondMain) {
		t.Fatalf("revision sources were not independently refreshed: first=%q second=%q workspace=%q", firstMain, secondMain, workspaceMain)
	}
	writeDeliverySourceCommit(t, ctx, source, "third")
	retry, err := manager.ensure(ctx, "session-1", "revision-2", source, "ticket-1")
	if err != nil {
		t.Fatal(err)
	}
	retryMain, err := gitOutput(ctx, retry.DeliverySource, "rev-parse", "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	if retry.DeliverySource != second.DeliverySource || strings.TrimSpace(retryMain) != strings.TrimSpace(secondMain) {
		t.Fatalf("revision retry source changed: path=%q main=%q", retry.DeliverySource, retryMain)
	}
}

func TestPrepareDeliveryWorkspaceReplacesAllOriginURLs(t *testing.T) {
	ctx := context.Background()
	repository := filepath.Join(t.TempDir(), "workspace")
	if err := runGit(ctx, "", "init", "-b", "main", repository); err != nil {
		t.Fatal(err)
	}
	for _, remoteURL := range []string{`C:\source\repository`, `D:\other\repository`} {
		if err := runGit(ctx, repository, "config", "--local", "--add", "remote.origin.url", remoteURL); err != nil {
			t.Fatal(err)
		}
	}
	if err := prepareDeliveryWorkspace(ctx, repository); err != nil {
		t.Fatal(err)
	}
	urls, err := gitOutput(ctx, repository, "config", "--local", "--get-all", "remote.origin.url")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(urls) != "/source-repository" {
		t.Fatalf("prepared Delivery Worker origins = %q", urls)
	}
}

func writeDeliverySourceCommit(t *testing.T, ctx context.Context, source, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(source, "source.txt"), []byte(content+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runGit(ctx, source, "add", "source.txt"); err != nil {
		t.Fatal(err)
	}
	if err := runGit(ctx, source, "commit", "-m", content); err != nil {
		t.Fatal(err)
	}
}
