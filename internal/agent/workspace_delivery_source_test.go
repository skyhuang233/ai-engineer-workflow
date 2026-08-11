package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/skyhuang233/workflow/internal/delivery"
	"github.com/skyhuang233/workflow/internal/worker"
)

type deliveryWorkspaceRuntimeFunc func(context.Context, worker.Spec) (worker.Result, error)

func (run deliveryWorkspaceRuntimeFunc) Run(ctx context.Context, spec worker.Spec) (worker.Result, error) {
	return run(ctx, spec)
}

func TestDeliverySourceAuthenticationFailure(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "credential rejection", err: delivery.ErrGatewayCredentialRejected, want: true},
		{name: "transient refresh", err: errors.New("network unavailable"), want: false},
		{name: "filesystem failure", err: os.ErrPermission, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isDeliverySourceAuthenticationFailure(test.err); got != test.want {
				t.Fatalf("isDeliverySourceAuthenticationFailure() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestDeliverySourcePathClassifiesOperationalAndIntegrityFailures(t *testing.T) {
	manager := WorkspaceManager{RootDir: "invalid\x00root"}
	_, err := manager.ensureDeliverySource(context.Background(), "session-1", "revision-1", "source")
	var infrastructureFailure *deliverySourceInfrastructureFailure
	if !errors.As(err, &infrastructureFailure) {
		t.Fatalf("operational path-resolution error = %T %v, want Delivery Source infrastructure failure", err, err)
	}

	manager.RootDir = t.TempDir()
	_, err = manager.ensureDeliverySource(context.Background(), "../session-1", "revision-1", "source")
	var integrityFailure *deliverySourceIntegrityFailure
	if !errors.As(err, &integrityFailure) {
		t.Fatalf("invalid managed ID error = %T %v, want Delivery Source integrity failure", err, err)
	}
}

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

func TestDeliverySourceUsesCanonicalDefaultBranchAndPinsRevision(t *testing.T) {
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
	if err := runGit(ctx, source, "checkout", "-b", "feature"); err != nil {
		t.Fatal(err)
	}
	writeDeliverySourceCommit(t, ctx, source, "feature")
	manager := WorkspaceManager{
		RootDir: filepath.Join(root, "workspaces"), CodexStateRoot: filepath.Join(root, "codex"),
		RefreshDeliverySource: func(ctx context.Context, snapshotPath string) (string, error) {
			headRef := "refs/heads/main"
			return headRef, runGit(ctx, snapshotPath, "fetch", "--force", "--no-tags", remote, "+"+headRef+":"+headRef)
		},
	}
	first, err := manager.ensure(ctx, "session-1", "revision-1", source, "ticket-1")
	if err != nil {
		t.Fatal(err)
	}
	firstHead, err := gitOutput(ctx, first.DeliverySource, "symbolic-ref", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(firstHead) != "refs/heads/main" {
		t.Fatalf("Delivery Source HEAD = %q, want canonical default branch", firstHead)
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

func TestReclaimSupersededDeliverySourcesKeepsAcceptedRevision(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := runGit(ctx, "", "init", "-b", "main", source); err != nil {
		t.Fatal(err)
	}
	configureDeliverySourceTestIdentity(t, ctx, source)
	writeDeliverySourceCommit(t, ctx, source, "first")
	manager := WorkspaceManager{RootDir: filepath.Join(root, "workspaces"), CodexStateRoot: filepath.Join(root, "codex")}
	first, err := manager.ensureDeliverySource(ctx, "session-1", "revision-1", source)
	if err != nil {
		t.Fatal(err)
	}
	writeDeliverySourceCommit(t, ctx, source, "second")
	current, err := manager.ensureDeliverySource(ctx, "session-1", "revision-2", source)
	if err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(filepath.Dir(current), ".delivery-orphan")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := manager.reclaimSupersededDeliverySources(ctx, "session-1", "revision-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(current); err != nil {
		t.Fatalf("accepted Delivery Source was reclaimed: %v", err)
	}
	for _, stale := range []string{first, orphan} {
		if _, err := os.Stat(stale); !os.IsNotExist(err) {
			t.Fatalf("superseded Delivery Source %q still exists: %v", stale, err)
		}
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

func TestRunInDeliveryWorkspaceClassifiesHostFailuresAsInfrastructure(t *testing.T) {
	ctx := context.Background()
	repository := filepath.Join(t.TempDir(), "workspace")
	if err := runGit(ctx, "", "init", "-b", "main", repository); err != nil {
		t.Fatal(err)
	}
	if err := runGit(ctx, repository, "config", "--local", "remote.origin.url", repository); err != nil {
		t.Fatal(err)
	}
	configLock := filepath.Join(repository, ".git", "config.lock")
	spec := worker.Spec{WorkspacePath: repository}

	t.Run("prepare", func(t *testing.T) {
		if err := os.WriteFile(configLock, []byte("locked"), 0o600); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(configLock)
		invoked := false
		_, err := runInDeliveryWorkspace(ctx, deliveryWorkspaceRuntimeFunc(func(context.Context, worker.Spec) (worker.Result, error) {
			invoked = true
			return worker.Result{}, nil
		}), spec, "")
		if err == nil || !worker.IsInfrastructureFailure(err) || invoked {
			t.Fatalf("preparation result: invoked=%t err=%v", invoked, err)
		}
	})

	t.Run("restore", func(t *testing.T) {
		result, err := runInDeliveryWorkspace(ctx, deliveryWorkspaceRuntimeFunc(func(context.Context, worker.Spec) (worker.Result, error) {
			if err := os.WriteFile(configLock, []byte("locked"), 0o600); err != nil {
				return worker.Result{}, err
			}
			return worker.Result{ContainerID: "container"}, nil
		}), spec, "")
		defer os.Remove(configLock)
		if result.ContainerID != "container" || err == nil || !worker.IsInfrastructureFailure(err) {
			t.Fatalf("restoration result: result=%#v err=%v", result, err)
		}
	})
}

func TestRevisionSourceApplicationClassifiesGitFailuresAsInfrastructure(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := runGit(ctx, "", "init", "-b", "main", source); err != nil {
		t.Fatal(err)
	}
	configureDeliverySourceTestIdentity(t, ctx, source)
	writeDeliverySourceCommit(t, ctx, source, "first")
	manager := WorkspaceManager{RootDir: filepath.Join(root, "workspaces"), CodexStateRoot: filepath.Join(root, "codex")}
	first, err := manager.ensure(ctx, "session-1", "revision-1", source, "ticket-1")
	if err != nil {
		t.Fatal(err)
	}
	configLock := filepath.Join(first.Path, ".git", "config.lock")
	if err := os.WriteFile(configLock, []byte("locked"), 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(configLock)
	_, err = manager.ensure(ctx, "session-1", "revision-2", source, "ticket-1")
	var infrastructureFailure *deliverySourceInfrastructureFailure
	if err == nil || !errors.As(err, &infrastructureFailure) {
		t.Fatalf("revision source application error = %v", err)
	}
}

func TestDeliverySourcePreflightDistinguishesInfrastructureFromIntegrity(t *testing.T) {
	ctx := context.Background()
	missing := filepath.Join(t.TempDir(), "missing.git")
	if _, err := trustedSourceDefaultBranch(ctx, missing); err == nil {
		t.Fatal("missing Delivery Source returned nil error")
	} else {
		var infrastructureFailure *deliverySourceInfrastructureFailure
		if !errors.As(err, &infrastructureFailure) {
			t.Fatalf("missing Delivery Source error = %v", err)
		}
	}

	source := filepath.Join(t.TempDir(), "source.git")
	if err := runGit(ctx, "", "init", "--bare", "--initial-branch", "main", source); err != nil {
		t.Fatal(err)
	}
	if err := runGit(ctx, source, "symbolic-ref", "HEAD", "refs/heads/missing"); err != nil {
		t.Fatal(err)
	}
	if _, err := trustedSourceDefaultBranch(ctx, source); err == nil {
		t.Fatal("invalid Delivery Source returned nil error")
	} else {
		var infrastructureFailure *deliverySourceInfrastructureFailure
		var integrityFailure *deliverySourceIntegrityFailure
		if errors.As(err, &infrastructureFailure) || !errors.As(err, &integrityFailure) {
			t.Fatalf("invalid Delivery Source error = %v", err)
		}
	}
}

func TestDeliverySourceDigestRejectsMissingReachableObjects(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := runGit(ctx, "", "init", "-b", "main", source); err != nil {
		t.Fatal(err)
	}
	configureDeliverySourceTestIdentity(t, ctx, source)
	writeDeliverySourceCommit(t, ctx, source, "first")
	manager := WorkspaceManager{RootDir: filepath.Join(root, "workspaces"), CodexStateRoot: filepath.Join(root, "codex")}
	deliverySource, err := manager.ensureDeliverySource(ctx, "session-1", "revision-1", source)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := gitOutput(ctx, deliverySource, "rev-parse", "refs/heads/main^{tree}")
	if err != nil {
		t.Fatal(err)
	}
	tree = strings.TrimSpace(tree)
	if err := os.Remove(filepath.Join(deliverySource, "objects", tree[:2], tree[2:])); err != nil {
		t.Fatal(err)
	}
	_, err = digestDeliverySource(ctx, deliverySource)
	var integrityFailure *deliverySourceIntegrityFailure
	if err == nil || !errors.As(err, &integrityFailure) {
		t.Fatalf("missing reachable object classification = %v", err)
	}
}

func TestDeliveryWorkspaceRestoresDurableAdmittedOriginAfterRetry(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := runGit(ctx, "", "init", "-b", "main", source); err != nil {
		t.Fatal(err)
	}
	configureDeliverySourceTestIdentity(t, ctx, source)
	writeDeliverySourceCommit(t, ctx, source, "first")
	manager := WorkspaceManager{RootDir: filepath.Join(root, "workspaces"), CodexStateRoot: filepath.Join(root, "codex")}
	workspace, err := manager.ensure(ctx, "session-1", "revision-1", source, "ticket-1")
	if err != nil {
		t.Fatal(err)
	}
	restore, err := prepareDeliveryWorkspace(ctx, workspace.Path, "")
	if err != nil {
		t.Fatal(err)
	}
	configLock := filepath.Join(workspace.Path, ".git", "config.lock")
	if err := os.WriteFile(configLock, []byte("locked"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := restore(ctx); err == nil {
		t.Fatal("locked origin restoration returned nil error")
	}
	if err := os.Remove(configLock); err != nil {
		t.Fatal(err)
	}
	admitted, err := admittedSourceRepository(ctx, workspace.Path, "")
	if err != nil {
		t.Fatal(err)
	}
	restore, err = prepareDeliveryWorkspace(ctx, workspace.Path, admitted)
	if err != nil {
		t.Fatal(err)
	}
	if err := restore(ctx); err != nil {
		t.Fatal(err)
	}
	origin, err := gitOutput(ctx, workspace.Path, "config", "--local", "--get-all", "remote.origin.url")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(origin) != source {
		t.Fatalf("restored Ticket Workspace origin = %q, want %q", origin, source)
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
