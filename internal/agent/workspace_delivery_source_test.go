package agent

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	if err := runGit(ctx, source, "tag", "v1"); err != nil {
		t.Fatal(err)
	}
	manager := WorkspaceManager{RootDir: filepath.Join(t.TempDir(), "workspaces"), CodexStateRoot: filepath.Join(t.TempDir(), "codex")}
	pinnedFirst, err := manager.ensureDeliverySource(ctx, "session-1", "revision-1", source)
	if err != nil {
		t.Fatal(err)
	}
	writeDeliverySourceCommit(t, ctx, source, "second")
	if err := runGit(ctx, source, "tag", "--delete", "v1"); err != nil {
		t.Fatal(err)
	}
	if err := runGit(ctx, source, "tag", "v2"); err != nil {
		t.Fatal(err)
	}
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
	assertGitRefExists(t, ctx, first.DeliverySource, "refs/tags/v1")
	assertGitRefExists(t, ctx, first.Path, "refs/tags/v1")
	assertGitRefMissing(t, ctx, first.Path, "refs/tags/v2")
	firstOrigin, err := gitOutput(ctx, first.Path, "config", "--local", "--get-all", "remote.origin.url")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(strings.TrimSpace(firstOrigin), source) {
		t.Fatalf("Ticket Workspace origin = %q, want admitted source %q", firstOrigin, source)
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
	assertGitRefMissing(t, ctx, second.DeliverySource, "refs/tags/v1")
	assertGitRefExists(t, ctx, second.DeliverySource, "refs/tags/v2")
	assertGitRefMissing(t, ctx, second.Path, "refs/tags/v1")
	assertGitRefExists(t, ctx, second.Path, "refs/tags/v2")
	writeDeliverySourceCommit(t, ctx, source, "third")
	if err := runGit(ctx, source, "tag", "--delete", "v2"); err != nil {
		t.Fatal(err)
	}
	if err := runGit(ctx, source, "tag", "v3"); err != nil {
		t.Fatal(err)
	}
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
	assertGitRefExists(t, ctx, retry.DeliverySource, "refs/tags/v2")
	assertGitRefMissing(t, ctx, retry.DeliverySource, "refs/tags/v3")
	assertGitRefExists(t, ctx, retry.Path, "refs/tags/v2")
	assertGitRefMissing(t, ctx, retry.Path, "refs/tags/v3")
}

func TestDeliverySourceRefreshesAdvancedRemoteBaseAndPinsRevision(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	if err := runGit(ctx, "", "init", "--bare", remote); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "source")
	if err := runGit(ctx, "", "init", "-b", "main", source); err != nil {
		t.Fatal(err)
	}
	configureDeliverySourceTestIdentity(t, ctx, source)
	writeDeliverySourceCommit(t, ctx, source, "first")
	if err := runGit(ctx, source, "remote", "add", "origin", remote); err != nil {
		t.Fatal(err)
	}
	if err := runGit(ctx, source, "push", "-u", "origin", "main"); err != nil {
		t.Fatal(err)
	}
	manager := WorkspaceManager{
		RootDir: filepath.Join(root, "workspaces"), CodexStateRoot: filepath.Join(root, "codex"),
		RefreshDeliverySource: func(ctx context.Context, snapshotPath, headRef string) error {
			return runGit(ctx, snapshotPath, "fetch", "--force", "--no-tags", remote, "+"+headRef+":"+headRef)
		},
	}
	first, err := manager.ensure(ctx, "session-1", "revision-1", source, "ticket-1")
	if err != nil {
		t.Fatal(err)
	}
	firstMain, err := gitOutput(ctx, first.DeliverySource, "rev-parse", "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	publisher := filepath.Join(root, "publisher")
	if err := runGit(ctx, "", "clone", "-b", "main", remote, publisher); err != nil {
		t.Fatal(err)
	}
	configureDeliverySourceTestIdentity(t, ctx, publisher)
	writeDeliverySourceCommit(t, ctx, publisher, "second")
	if err := runGit(ctx, publisher, "push", "origin", "main"); err != nil {
		t.Fatal(err)
	}
	sourceMain, err := gitOutput(ctx, source, "rev-parse", "refs/heads/main")
	if err != nil {
		t.Fatal(err)
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
	if strings.TrimSpace(sourceMain) != strings.TrimSpace(firstMain) || strings.TrimSpace(secondMain) == strings.TrimSpace(firstMain) || strings.TrimSpace(workspaceMain) != strings.TrimSpace(secondMain) {
		t.Fatalf("advanced base was not refreshed through the pinned snapshot: source=%q first=%q second=%q workspace=%q", sourceMain, firstMain, secondMain, workspaceMain)
	}
	writeDeliverySourceCommit(t, ctx, publisher, "third")
	if err := runGit(ctx, publisher, "push", "origin", "main"); err != nil {
		t.Fatal(err)
	}
	retry, err := manager.ensure(ctx, "session-1", "revision-2", source, "ticket-1")
	if err != nil {
		t.Fatal(err)
	}
	retryMain, err := gitOutput(ctx, retry.DeliverySource, "rev-parse", "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(retryMain) != strings.TrimSpace(secondMain) {
		t.Fatalf("revision retry refreshed pinned base: retry=%q second=%q", retryMain, secondMain)
	}
}

func TestDeliverySourceReadableFromLinuxDockerOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("requires a Windows host")
	}
	ctx := context.Background()
	dockerInfo := exec.CommandContext(ctx, "docker", "info", "--format", "{{.OSType}}")
	output, err := dockerInfo.CombinedOutput()
	if err != nil {
		t.Skipf("Docker is unavailable: %v", err)
	}
	if strings.TrimSpace(string(output)) != "linux" {
		t.Skip("requires Docker Desktop with Linux containers")
	}
	source := filepath.Join(t.TempDir(), "source")
	if err := runGit(ctx, "", "init", "-b", "main", source); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{"user.name": "Test", "user.email": "test@example.com"} {
		if err := runGit(ctx, source, "config", key, value); err != nil {
			t.Fatal(err)
		}
	}
	writeDeliverySourceCommit(t, ctx, source, "mounted")
	manager := WorkspaceManager{RootDir: filepath.Join(t.TempDir(), "workspaces"), CodexStateRoot: filepath.Join(t.TempDir(), "codex")}
	deliverySource, err := manager.ensureDeliverySource(ctx, "session-1", "revision-1", source)
	if err != nil {
		t.Fatal(err)
	}
	want, err := gitOutput(ctx, deliverySource, "rev-parse", "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	configBytes, err := os.ReadFile(filepath.Join("..", "..", "config", "toolchain.json"))
	if err != nil {
		t.Fatal(err)
	}
	var toolchain struct {
		Worker struct {
			Version         string `json:"version"`
			ImageRepository string `json:"image_repository"`
		} `json:"worker"`
	}
	if err := json.Unmarshal(configBytes, &toolchain); err != nil {
		t.Fatal(err)
	}
	image := toolchain.Worker.ImageRepository + ":" + toolchain.Worker.Version
	mount := "type=bind,source=" + deliverySource + ",target=/source-repository,readonly"
	command := exec.CommandContext(ctx, "docker", "run", "--rm", "--mount", mount, "--entrypoint", "sh", image, "-ceu", "git init -q /tmp/checkout && git -C /tmp/checkout fetch -q /source-repository refs/heads/main && git -C /tmp/checkout rev-parse FETCH_HEAD")
	output, err = command.CombinedOutput()
	if err != nil {
		t.Fatalf("fetch Delivery Source through Linux Worker mount: %v\n%s", err, output)
	}
	if strings.TrimSpace(string(output)) != strings.TrimSpace(want) {
		t.Fatalf("Linux Worker fetched %q, want %q", output, want)
	}
}

func TestPrepareDeliveryWorkspaceScopesOriginReplacement(t *testing.T) {
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
	restore, err := prepareDeliveryWorkspace(ctx, repository, `C:\source\repository`)
	if err != nil {
		t.Fatal(err)
	}
	urls, err := gitOutput(ctx, repository, "config", "--local", "--get-all", "remote.origin.url")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(urls) != "/source-repository" {
		t.Fatalf("prepared Delivery Worker origins = %q", urls)
	}
	if err := restore(ctx); err != nil {
		t.Fatal(err)
	}
	urls, err = gitOutput(ctx, repository, "config", "--local", "--get-all", "remote.origin.url")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(urls) != `C:\source\repository` {
		t.Fatalf("restored Ticket Workspace origins = %q", urls)
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

func configureDeliverySourceTestIdentity(t *testing.T, ctx context.Context, repository string) {
	t.Helper()
	for key, value := range map[string]string{"user.name": "Test", "user.email": "test@example.com"} {
		if err := runGit(ctx, repository, "config", key, value); err != nil {
			t.Fatal(err)
		}
	}
}

func assertGitRefExists(t *testing.T, ctx context.Context, repository, ref string) {
	t.Helper()
	if _, err := gitOutput(ctx, repository, "rev-parse", "--verify", ref); err != nil {
		t.Fatalf("Git ref %s is missing from %s: %v", ref, repository, err)
	}
}

func assertGitRefMissing(t *testing.T, ctx context.Context, repository, ref string) {
	t.Helper()
	if _, err := gitOutput(ctx, repository, "rev-parse", "--verify", ref); err == nil {
		t.Fatalf("Git ref %s unexpectedly exists in %s", ref, repository)
	}
}
