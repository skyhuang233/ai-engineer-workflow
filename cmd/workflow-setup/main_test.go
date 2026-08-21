package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skyhuang233/workflow/internal/controlplane"
	"github.com/skyhuang233/workflow/internal/launcher"
	"github.com/skyhuang233/workflow/internal/platformrelease"
	"github.com/skyhuang233/workflow/internal/workflowhome"
	"github.com/skyhuang233/workflow/internal/workflowrelease"
)

type packagedLifecycle struct{}

// DockerVersion pins the packaged test's initial consent to an observed
// synthetic Docker Desktop product version.  The child launcher deliberately
// uses its real Windows dependency inspector, so its install/reuse target can
// never equal this record, independent of the CI host.
func (packagedLifecycle) DockerVersion(context.Context) (string, error) {
	return "0.0.0-packaged-test", nil
}

func (packagedLifecycle) Prepare(context.Context, launcher.Request, launcher.Consent) error {
	return nil
}
func (packagedLifecycle) Stop(context.Context, string, launcher.Active) error  { return nil }
func (packagedLifecycle) Start(context.Context, string, launcher.Active) error { return nil }
func (packagedLifecycle) Ready(context.Context, string, launcher.Active) error { return nil }

func TestDispatcherForwardsOrdinaryCommandStandardInput(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	generation := strings.Repeat("a", 64)
	versioned := filepath.Join(home, "platform", "generations", generation, "workflow.exe")
	if err := os.MkdirAll(filepath.Dir(versioned), 0o700); err != nil {
		t.Fatal(err)
	}
	helperSource := filepath.Join(root, "stdin-echo.go")
	if err := os.WriteFile(helperSource, []byte(`package main
import ("io"; "os")
func main() { _, _ = io.Copy(os.Stdout, os.Stdin) }
`), 0o600); err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-o", versioned, helperSource)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build stdin helper: %v\n%s", err, output)
	}
	active := launcher.Active{SchemaVersion: 1, Generation: generation, Version: "0.0.1", BundleDigest: "sha256:" + generation, AttemptID: "attempt", ConsentID: "consent", Readiness: "ready"}
	raw, err := json.Marshal(active)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "platform"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "platform", "active.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	input := []byte(`{"exact":"onboarding-plan"}`)
	var output bytes.Buffer
	if err := dispatch([]string{"echo", "--workflow-home", home}, bytes.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output.Bytes(), input) {
		t.Fatalf("ordinary command stdin = %q, want %q", output.Bytes(), input)
	}
}

func TestPackagedGenerationLauncherSurvivesBundleCleanupThroughDispatcher(t *testing.T) {
	root := t.TempDir()
	launcherSource := filepath.Join(root, "workflow-setup.exe")
	workflowSource := filepath.Join(root, "workflow.exe")
	build := exec.Command("go", "build", "-o", launcherSource, ".")
	build.Dir = "."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build packaged launcher: %v\n%s", err, output)
	}
	launcherBytes, err := os.ReadFile(launcherSource)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workflowSource, launcherBytes, 0o700); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(root, "payload")
	for name, data := range map[string]string{"skills/agent-workflow/SKILL.md": "skill\n", "repository-contract/repository.json": "{}\n"} {
		path := filepath.Join(payload, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	bundle := filepath.Join(root, "workflow-windows-amd64.zip")
	manifest := platformrelease.BundleManifest{SchemaVersion: 1, SetupProtocolVersion: 1, Version: "0.0.1", Compatibility: platformrelease.Compatibility{OS: "windows", Architecture: "amd64", DatabaseSchema: 63, DockerDesktopVersion: "4.86.0", DockerInstallerURL: "https://example.test/docker.exe", DockerInstallerSHA256: strings.Repeat("b", 64), WorkerImage: "ghcr.io/skyhuang233/workflow-worker@sha256:" + strings.Repeat("a", 64)}}
	if err := platformrelease.AssembleBundle(platformrelease.BundleAssembleOptions{Output: bundle, SetupExecutable: launcherSource, WorkflowExecutable: workflowSource, PayloadDirectory: payload, Manifest: manifest}); err != nil {
		t.Fatal(err)
	}
	extracted := filepath.Join(root, "extracted")
	extractBundle(t, bundle, extracted)
	home := filepath.Join(root, "home")
	digestBytes, err := os.ReadFile(bundle)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(digestBytes)
	bundleDigest := "sha256:" + hex.EncodeToString(digest[:])
	releaseManifest := workflowrelease.Manifest{
		SchemaVersion: 1, Version: "0.0.1", CandidateSourceCommit: strings.Repeat("c", 40), QualificationRunID: 1, QualificationRunAttempt: 1,
		Bundle: workflowrelease.Bundle{Name: workflowrelease.BundleAssetName, SHA256: strings.TrimPrefix(bundleDigest, "sha256:")},
		Worker: workflowrelease.Worker{Image: manifest.Compatibility.WorkerImage, Tools: workflowrelease.Tools{
			Codex: workflowrelease.CodexTool{Version: "0.148.0"}, GitHubCLI: workflowrelease.ArchiveTool{Version: "2.97.0", LinuxAMD64SHA256: strings.Repeat("d", 64)},
			Go: workflowrelease.ArchiveTool{Version: "1.26.6", LinuxAMD64SHA256: strings.Repeat("e", 64)}, NoMistakes: workflowrelease.NoMistakesTool{Version: "v1.41.2", Repository: "skyhuang233/no-mistakes", Commit: strings.Repeat("f", 40)},
		}},
		SBOM: workflowrelease.SBOM{Name: workflowrelease.SBOMAssetName, Format: "spdx-json", SHA256: strings.Repeat("3", 64), Scan: workflowrelease.Scan{Scanner: "grype", SeverityCutoff: "high", OnlyFixed: true}},
	}
	releaseRaw, err := releaseManifest.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	releasePath := filepath.Join(root, workflowrelease.ManifestAssetName)
	if err := os.WriteFile(releasePath, releaseRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	releaseDigest := sha256.Sum256(releaseRaw)
	verifiedRelease := &launcher.VerifiedReleaseManifest{ManifestPath: releasePath, ManifestSHA256: hex.EncodeToString(releaseDigest[:]), SourceCommit: releaseManifest.CandidateSourceCommit}
	engine := launcher.Engine{BundleRoot: extracted, Lifecycle: packagedLifecycle{}, DependencyInspector: packagedLifecycle{}}
	inspectRequest := launcher.Request{SchemaVersion: launcher.ProtocolVersion, Operation: launcher.Inspect, WorkflowHome: home, Purpose: launcher.PurposeTargetState, TargetVersion: "0.0.1", BundleDigest: bundleDigest, GitHubOwner: "owner", VerifiedReleaseManifest: verifiedRelease}
	inspection, err := engine.Inspect(context.Background(), inspectRequest)
	if err != nil {
		t.Fatal(err)
	}
	capabilities, ok := inspection.Evidence["required_capabilities"].([]launcher.Capability)
	if inspection.Status != "consent_required" || !ok || len(capabilities) == 0 {
		t.Fatalf("packaged inspect=%#v", inspection)
	}
	request := launcher.Request{SchemaVersion: launcher.ProtocolVersion, Operation: launcher.Apply, WorkflowHome: home, TargetVersion: "0.0.1", BundleDigest: bundleDigest, GitHubOwner: "owner", AcceptedCapabilities: capabilities, VerifiedReleaseManifest: verifiedRelease}
	if result, err := engine.Apply(context.Background(), request); err != nil || result.Status != "ready" {
		t.Fatalf("fresh apply=%#v, %v", result, err)
	}
	if err := os.RemoveAll(extracted); err != nil {
		t.Fatal(err)
	}
	active, err := launcher.ReadActive(home)
	if err != nil {
		t.Fatal(err)
	}
	serveHealthyControlPlane(t, home, active)

	inspect := launcher.Request{SchemaVersion: launcher.ProtocolVersion, Operation: launcher.Inspect, WorkflowHome: home, Purpose: launcher.PurposeTargetState, TargetVersion: active.Version, BundleDigest: active.BundleDigest, GitHubOwner: "owner", VerifiedReleaseManifest: verifiedRelease}
	inspectResult := runDispatcherSetup(t, home, "inspect", inspect)
	// This synthetic packaged setup did not actually install the Bundle's
	// Docker version. Dispatcher inspect must therefore surface replacement
	// consent rather than blindly reusing the generic prior record.
	if inspectResult.Status != "consent_required" {
		t.Fatalf("dispatcher inspect=%#v", inspectResult)
	}
	verify := launcher.Request{SchemaVersion: launcher.ProtocolVersion, Operation: launcher.Verify, WorkflowHome: home}
	verifyResult := runDispatcherSetup(t, home, "verify", verify)
	if verifyResult.Status != "platform_ready" {
		t.Fatalf("dispatcher verify=%#v", verifyResult)
	}
}

func extractBundle(t *testing.T, archivePath, destination string) {
	t.Helper()
	archive, err := zip.OpenReader(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	for _, entry := range archive.File {
		path := filepath.Join(destination, filepath.FromSlash(entry.Name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		reader, err := entry.Open()
		if err != nil {
			t.Fatal(err)
		}
		data := new(bytes.Buffer)
		_, copyErr := data.ReadFrom(reader)
		closeErr := reader.Close()
		if copyErr != nil || closeErr != nil {
			t.Fatalf("extract %s: %v %v", entry.Name, copyErr, closeErr)
		}
		if err := os.WriteFile(path, data.Bytes(), 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

func serveHealthyControlPlane(t *testing.T, home string, active launcher.Active) {
	t.Helper()
	started, live, err := controlplane.OSProcessIdentity(os.Getpid())
	if err != nil || !live {
		t.Fatalf("current process identity: %v, live=%v", err, live)
	}
	record := controlplane.RuntimeRecord{PID: os.Getpid(), PlatformVersion: active.Version, ProcessStartedAt: started, Generation: active.Generation, BundleDigest: strings.TrimPrefix(active.BundleDigest, "sha256:")}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/health" {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(writer).Encode(controlplane.Health{Status: "ready", Identity: record.Identity()})
	}))
	t.Cleanup(server.Close)
	record.Endpoints = controlplane.Endpoints{Health: server.URL + "/health", Shutdown: server.URL + "/shutdown"}
	layout, err := workflowhome.Resolve(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := controlplane.WriteRuntimeRecord(layout, record); err != nil {
		t.Fatal(err)
	}
}

func runDispatcherSetup(t *testing.T, home, operation string, request launcher.Request) launcher.Result {
	t.Helper()
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(filepath.Join(home, "bin", "workflow.exe"), "setup", operation)
	command.Stdin = bytes.NewReader(raw)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("dispatcher %s: %v\n%s", operation, err, output)
	}
	var result launcher.Result
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode dispatcher %s: %v\n%s", operation, err, output)
	}
	return result
}
