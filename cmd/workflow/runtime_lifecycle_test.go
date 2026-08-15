package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/skyhuang233/workflow/internal/controlplane"
	"github.com/skyhuang233/workflow/internal/credential"
	"github.com/skyhuang233/workflow/internal/platformrelease"
	"github.com/skyhuang233/workflow/internal/store"
	"github.com/skyhuang233/workflow/internal/workflowhome"
)

func TestRuntimeStatusReportsStoppedWithoutRecord(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	var output bytes.Buffer
	if err := runtimeStatusCommand([]string{"--workflow-home", home}, &output); err != nil {
		t.Fatal(err)
	}
	var observation controlplane.Observation
	if err := json.Unmarshal(output.Bytes(), &observation); err != nil {
		t.Fatal(err)
	}
	if observation.State != controlplane.StateStopped {
		t.Fatalf("observation = %#v", observation)
	}
}

func TestRuntimeLogsTailsOnlyRequestedLinesFromManagedPaths(t *testing.T) {
	layout, err := workflowhome.Resolve(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	stdout, stderr := controlplane.LogPaths(layout)
	if err := os.WriteFile(stdout, []byte("one\ntwo\nthree\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stderr, []byte("error-one\nerror-two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runtimeLogsCommand([]string{"--workflow-home", layout.Root, "--lines", "1"}, &output); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	if strings.Contains(got, "\none\n") || strings.Contains(got, "\ntwo\n") || strings.Contains(got, "\nerror-one\n") || !strings.Contains(got, "\nthree\n") || !strings.Contains(got, "\nerror-two\n") {
		t.Fatalf("tail = %q", got)
	}
}

func TestForegroundChildCannotRecursivelyDetach(t *testing.T) {
	if err := serveChildCommand([]string{"--detach"}); err == nil {
		t.Fatal("hidden child accepted recursive detach input")
	}
}

func TestForegroundChildRejectsRuntimePlatformVersionOverride(t *testing.T) {
	err := serveChildCommand([]string{"--workflow-home", filepath.Join(t.TempDir(), "home"), "--listen", "127.0.0.1:0", "--platform-version", "forged", "--approved-plan-digest", strings.Repeat("a", 64)})
	if err == nil || !strings.Contains(err.Error(), "platform-version") {
		t.Fatalf("hidden child accepted a runtime platform version override: %v", err)
	}
}

func TestForegroundChildRejectsDurablePlatformVersionDifferentFromBuild(t *testing.T) {
	layout, err := workflowhome.Resolve(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(context.Background(), filepath.Join(layout.State, "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("a", 64)
	bundleJSON, bundleDigest := testReleaseBundle(strings.Repeat("d", 64))
	installation := store.PlatformInstallation{PlatformVersion: "1.0.0", ReleaseManifestDigestSHA256: strings.Repeat("b", 64), PlatformSetupContractDigestSHA256: strings.Repeat("c", 64), WorkflowCLISHA256: strings.Repeat("d", 64), ReleaseBundledFilesJSON: bundleJSON, ReleaseBundledFilesDigestSHA256: bundleDigest, ControlPlanePlanDigestSHA256: digest, WorkflowHome: layout.Root, InstalledAt: time.Now().UTC(), VerifiedAt: time.Now().UTC()}
	if err := database.RecordPlatformInstallation(context.Background(), installation); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	err = serveChildCommand([]string{"--workflow-home", layout.Root, "--listen", "127.0.0.1:0", "--approved-plan-digest", digest})
	if err == nil || !strings.Contains(err.Error(), `published version "dev" differs`) {
		t.Fatalf("development binary impersonated a published installation: %v", err)
	}
}

func TestServeLauncherAndChildRejectSameVersionExecutableWithDifferentChecksum(t *testing.T) {
	layout, err := workflowhome.Resolve(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(context.Background(), filepath.Join(layout.State, "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("a", 64)
	bundleJSON, bundleDigest := testReleaseBundle(strings.Repeat("d", 64))
	installation := store.PlatformInstallation{PlatformVersion: Version, ReleaseManifestDigestSHA256: strings.Repeat("b", 64), PlatformSetupContractDigestSHA256: strings.Repeat("c", 64), WorkflowCLISHA256: strings.Repeat("1", 64), ReleaseBundledFilesJSON: bundleJSON, ReleaseBundledFilesDigestSHA256: bundleDigest, ControlPlanePlanDigestSHA256: digest, WorkflowHome: layout.Root, InstalledAt: time.Now().UTC(), VerifiedAt: time.Now().UTC()}
	if err := database.RecordPlatformInstallation(context.Background(), installation); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	previousDigest := workflowExecutableSHA256
	workflowExecutableSHA256 = func() (string, error) { return strings.Repeat("2", 64), nil }
	t.Cleanup(func() { workflowExecutableSHA256 = previousDigest })

	for _, invoke := range []struct {
		name string
		run  func() error
	}{
		{name: "launcher", run: func() error {
			return serveCommand([]string{"--workflow-home", layout.Root, "--approved-plan-digest", digest}, io.Discard)
		}},
		{name: "child", run: func() error {
			return serveChildCommand([]string{"--workflow-home", layout.Root, "--listen", "127.0.0.1:0", "--approved-plan-digest", digest})
		}},
	} {
		t.Run(invoke.name, func(t *testing.T) {
			err := invoke.run()
			if err == nil || !strings.Contains(err.Error(), "checksum differs") {
				t.Fatalf("same-version executable checksum mismatch was accepted: %v", err)
			}
			if _, recordErr := controlplane.ReadRuntimeRecord(layout); !errors.Is(recordErr, os.ErrNotExist) {
				t.Fatalf("checksum mismatch wrote a runtime record: %v", recordErr)
			}
		})
	}
}

func TestRuntimeConfigureCompletesDurableRepositoryConfiguration(t *testing.T) {
	ctx := context.Background()
	layout, err := workflowhome.Resolve(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(ctx, filepath.Join(layout.State, "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := database.RecordRepositoryAdmission(ctx, store.RepositoryAdmission{Repository: "owner/repo", OnboardingPlanDigestSHA256: strings.Repeat("a", 64), ContractVersion: "1", ManifestDigestSHA256: strings.Repeat("b", 64), Eligible: true, VerifiedAt: now}); err != nil {
		database.Close()
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "repo")
	if err := database.RecordRepositoryRuntimeConfiguration(ctx, store.RepositoryRuntimeConfiguration{Repository: "owner/repo", DefaultBranch: "main", SourcePath: source, GitHubAPIURL: "https://api.github.com", PollInterval: time.Minute, WorkspaceRetention: 7 * 24 * time.Hour, MaxParallelRuns: 1, UpdatedAt: now}); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	authFile := filepath.Join(t.TempDir(), "supported-auth-source.json")
	if err := os.WriteFile(authFile, []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"access","account_id":"account","id_token":"id","refresh_token":"refresh"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runtimeConfigureCommand([]string{"--workflow-home", layout.Root, "--source", source, "--root", "42", "--max-parallel-runs", "2", "--codex-auth-file", authFile}, &output); err != nil {
		t.Fatal(err)
	}
	database, err = store.Open(ctx, filepath.Join(layout.State, "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	config, err := database.RepositoryRuntimeConfiguration(ctx, "owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Ready(); err != nil {
		t.Fatalf("configured runtime is not ready: %#v, %v", config, err)
	}
	if config.RootIssueNumber != 42 || config.MaxParallelRuns != 2 || config.SourcePath != source || config.CodexAuthFile != authFile {
		t.Fatalf("configured runtime = %#v", config)
	}
}

func TestWindowsDetachedServeSurvivesLauncherAndDoesNotRestartAfterStop(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows process lifetime contract")
	}
	directory := t.TempDir()
	executable := filepath.Join(directory, "workflow.exe")
	build := exec.Command("go", "build", "-ldflags", "-X main.Version=1.0.0", "-o", executable, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build workflow: %v\n%s", err, output)
	}
	executableBytes, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	executableDigest := sha256.Sum256(executableBytes)
	layout, err := workflowhome.Resolve(filepath.Join(directory, "home"))
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(context.Background(), filepath.Join(layout.State, "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	digest := strings.Repeat("a", 64)
	bundleJSON, bundleDigest := testReleaseBundle(strings.Repeat("d", 64))
	if err := database.RecordPlatformInstallation(context.Background(), store.PlatformInstallation{PlatformVersion: "1.0.0", ReleaseManifestDigestSHA256: strings.Repeat("b", 64), PlatformSetupContractDigestSHA256: strings.Repeat("c", 64), WorkflowCLISHA256: hex.EncodeToString(executableDigest[:]), ReleaseBundledFilesJSON: bundleJSON, ReleaseBundledFilesDigestSHA256: bundleDigest, ControlPlanePlanDigestSHA256: digest, WorkflowHome: layout.Root, InstalledAt: now, VerifiedAt: now}); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.RecordSetupPlan(context.Background(), store.SetupPlanRecord{PlanID: "platform", Kind: "platform_bootstrap", SchemaVersion: 1, Target: layout.Root, DigestSHA256: digest, CanonicalJSON: `{}`, Projection: "platform", CreatedAt: now}); err != nil {
		database.Close()
		t.Fatal(err)
	}
	token := "test-pat-not-sent-without-an-admission"
	if err := credential.NewFileStore(layout.CredentialFile).Set(context.Background(), credential.GatewayTarget, token); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.RecordGitHubPATVerification(context.Background(), store.GitHubPATVerification{FingerprintSHA256: credential.Fingerprint(token), Login: "owner", UserID: 1, Owner: "owner", Scopes: []string{"repo", "workflow"}, CredentialPath: layout.CredentialFile, Status: "verified", VerifiedAt: now}); err != nil {
		database.Close()
		t.Fatal(err)
	}
	manifestRaw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "platform", "release-manifest.json"))
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	var release struct {
		Contract platformrelease.PlatformSetupContract `json:"platform_setup_contract"`
	}
	if err := json.Unmarshal(manifestRaw, &release); err != nil {
		database.Close()
		t.Fatal(err)
	}
	contractRaw, _ := json.Marshal(release.Contract)
	if err := os.WriteFile(filepath.Join(layout.Config, "platform-setup-contract.json"), contractRaw, 0o600); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	run := func(arguments ...string) ([]byte, error) {
		command := exec.Command(executable, arguments...)
		return command.CombinedOutput()
	}
	output, err := run("serve", "--workflow-home", layout.Root, "--startup-timeout", "10s")
	if err != nil {
		t.Fatalf("serve: %v\n%s", err, output)
	}
	record, err := controlplane.ReadRuntimeRecord(layout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = controlplane.Stop(ctx, record, controlplane.Inspector{})
	})
	if observation := (controlplane.Inspector{}).Inspect(context.Background(), &record); observation.State != controlplane.StateReady {
		t.Fatalf("detached child = %#v", observation)
	}
	output, err = run("stop", "--workflow-home", layout.Root, "--timeout", "10s")
	if err != nil {
		t.Fatalf("stop: %v\n%s", err, output)
	}
	time.Sleep(300 * time.Millisecond)
	_, live, err := controlplane.OSProcessIdentity(record.PID)
	if err != nil || live {
		t.Fatalf("process auto-restarted or remained live: live=%v err=%v", live, err)
	}
}
