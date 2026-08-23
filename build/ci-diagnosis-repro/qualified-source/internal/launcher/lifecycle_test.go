package launcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skyhuang233/workflow/internal/hostsetup"
)

type recordingRunner struct {
	calls         [][]string
	dockerVersion string
}

func (r *recordingRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	if name == "docker" && len(args) > 0 && args[0] == "version" {
		return []byte(r.dockerVersion + "\n"), nil
	}
	return []byte("ok"), nil
}

type fakeDockerDesktopHost struct {
	version                            string
	installVersion                     string
	download                           []byte
	downloads, installs, starts, ready int
	readyAfterStart                    bool
}

func (h *fakeDockerDesktopHost) InstalledVersion(context.Context) (string, error) {
	return h.version, nil
}
func (h *fakeDockerDesktopHost) Download(_ context.Context, _ string, path string) error {
	h.downloads++
	return os.WriteFile(path, h.download, 0o600)
}
func (h *fakeDockerDesktopHost) InstallElevated(context.Context, string) error {
	h.installs++
	h.version = h.installVersion
	return nil
}
func (h *fakeDockerDesktopHost) Start(context.Context) error {
	h.starts++
	h.readyAfterStart = true
	return nil
}
func (h *fakeDockerDesktopHost) EngineReady(context.Context) error {
	h.ready++
	if h.readyAfterStart {
		return nil
	}
	return errors.New("Docker daemon is stopped")
}

func dockerTarget(action, observed string) DockerCapabilityTarget {
	return DockerCapabilityTarget{Action: action, RequiredVersion: "4.86.0", ObservedVersion: observed, InstallerURL: "https://example.test/docker.exe", InstallerSHA256: strings.Repeat("a", 64), HostImpact: "test Docker Desktop"}
}

func TestWindowsDependenciesReadsDesktopDisplayVersionNotEngineVersion(t *testing.T) {
	runner := &recordingRunner{dockerVersion: "29.7.2"}
	host := &fakeDockerDesktopHost{version: "4.86.0"}
	dependencies := WindowsDependencies{Runner: runner, DockerHost: host}
	got, err := dependencies.DockerVersion(context.Background())
	if err != nil || got != "4.86.0" {
		t.Fatalf("Docker Desktop version=%q,%v", got, err)
	}
	for _, call := range runner.calls {
		if call[0] == "docker" && len(call) > 1 && call[1] == "version" {
			t.Fatalf("Engine version command was used as Desktop version: %#v", runner.calls)
		}
	}
}

func TestLauncherInspectBuildsReuseConsentFromDesktopDisplayVersion(t *testing.T) {
	bundle := t.TempDir()
	writeTestBundle(t, bundle, map[string]string{"platform/workflow.exe": "cli", "setup/workflow-setup.exe": "setup", "skills/agent-workflow/SKILL.md": "skill", "repository-contract/repository.json": "contract"})
	runner := &recordingRunner{dockerVersion: "29.7.2"}
	dependencies := WindowsDependencies{Runner: runner, DockerHost: &fakeDockerDesktopHost{version: "4.86.0"}}
	engine := Engine{BundleRoot: bundle, DependencyInspector: dependencies}
	request := Request{SchemaVersion: ProtocolVersion, Operation: Inspect, Purpose: PurposeTargetState, WorkflowHome: filepath.Join(t.TempDir(), "home"), TargetVersion: "0.0.1", BundleDigest: "sha256:" + strings.Repeat("a", 64), GitHubOwner: "owner"}
	capabilities, err := engine.requiredCapabilities(context.Background(), request, nil)
	if err != nil {
		t.Fatal(err)
	}
	target, err := dockerCapability(capabilities)
	if err != nil || target.Action != dockerActionReuse || target.ObservedVersion != "4.86.0" {
		t.Fatalf("inspect Docker capability=%#v,%v", target, err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("inspect used Engine version rather than Desktop registry: %#v", runner.calls)
	}
}

func TestWindowsDependenciesExactReuseStartsAndWaitsForStoppedDaemon(t *testing.T) {
	host := &fakeDockerDesktopHost{version: "4.86.0"}
	dependencies := WindowsDependencies{DockerHost: host, TemporaryRoot: t.TempDir(), DockerTimeout: time.Second}
	target := dockerTarget(dockerActionReuse, "4.86.0")
	target.InstallerURL, target.InstallerSHA256 = "", ""
	if err := dependencies.EnsureDocker(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	if host.downloads != 0 || host.installs != 0 || host.starts != 1 || host.ready == 0 {
		t.Fatalf("reuse lifecycle download=%d install=%d start=%d ready=%d", host.downloads, host.installs, host.starts, host.ready)
	}
}

func TestWindowsDependenciesReuseDriftFailsClosedBeforeHostMutation(t *testing.T) {
	host := &fakeDockerDesktopHost{version: "4.85.0"}
	dependencies := WindowsDependencies{DockerHost: host, TemporaryRoot: t.TempDir(), DockerTimeout: time.Second}
	target := dockerTarget(dockerActionReuse, "4.86.0")
	target.InstallerURL, target.InstallerSHA256 = "", ""
	if err := dependencies.EnsureDocker(context.Background(), target); err == nil {
		t.Fatal("reuse accepted a changed Docker Desktop version")
	}
	if host.downloads != 0 || host.installs != 0 || host.starts != 0 {
		t.Fatalf("reuse drift mutated host: download=%d install=%d start=%d", host.downloads, host.installs, host.starts)
	}
}

func TestWindowsDependenciesInstallReadsBackExactDesktopVersionThenStartsDaemon(t *testing.T) {
	installer := []byte("verified Docker Desktop installer")
	sum := sha256.Sum256(installer)
	host := &fakeDockerDesktopHost{installVersion: "4.86.0", download: installer}
	dependencies := WindowsDependencies{DockerHost: host, TemporaryRoot: t.TempDir(), DockerTimeout: time.Second}
	target := dockerTarget(dockerActionInstall, "")
	target.InstallerSHA256 = hex.EncodeToString(sum[:])
	if err := dependencies.EnsureDocker(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	if host.downloads != 1 || host.installs != 1 || host.starts != 1 || host.ready == 0 {
		t.Fatalf("install lifecycle download=%d install=%d start=%d ready=%d", host.downloads, host.installs, host.starts, host.ready)
	}
}

func TestWindowsDependenciesInstallRejectsWrongDesktopReadbackBeforeDaemonStart(t *testing.T) {
	installer := []byte("verified Docker Desktop installer")
	sum := sha256.Sum256(installer)
	host := &fakeDockerDesktopHost{installVersion: "4.85.0", download: installer}
	dependencies := WindowsDependencies{DockerHost: host, TemporaryRoot: t.TempDir(), DockerTimeout: time.Second}
	target := dockerTarget(dockerActionUpgrade, "4.85.0")
	target.InstallerSHA256 = hex.EncodeToString(sum[:])
	if err := dependencies.EnsureDocker(context.Background(), target); err == nil {
		t.Fatal("install accepted a mismatched Desktop registry readback")
	}
	if host.starts != 0 || host.ready != 0 {
		t.Fatalf("mismatched install started daemon: start=%d ready=%d", host.starts, host.ready)
	}
}

var _ hostsetup.DockerDesktopHost = (*fakeDockerDesktopHost)(nil)

func TestWindowsDependenciesReusesDockerAndPullsExactImage(t *testing.T) {
	runner := &recordingRunner{}
	dependencies := WindowsDependencies{Runner: runner, DockerHost: &fakeDockerDesktopHost{version: "4.86.0"}, DockerTimeout: time.Second}
	image := "ghcr.io/owner/worker@sha256:" + strings.Repeat("a", 64)
	docker := DockerCapabilityTarget{Action: dockerActionReuse, RequiredVersion: "4.86.0", ObservedVersion: "4.86.0", HostImpact: "reuse verified Docker Desktop"}
	if err := dependencies.EnsureDocker(context.Background(), docker); err != nil {
		t.Fatal(err)
	}
	if err := dependencies.EnsureWorkerImage(context.Background(), WorkerImageCapabilityTarget{Image: image}); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 || runner.calls[0][0] != "docker" || runner.calls[0][1] != "pull" || runner.calls[0][2] != image || runner.calls[1][1] != "image" {
		t.Fatalf("dependency calls = %#v", runner.calls)
	}
}

func TestWindowsControlPlaneStartsGenerationLocalExecutable(t *testing.T) {
	home := t.TempDir()
	active := Active{Generation: strings.Repeat("b", 64), Version: "0.0.1"}
	var gotExecutable string
	var gotArgs []string
	plane := WindowsControlPlane{StartProcess: func(_ context.Context, executable string, args ...string) error {
		gotExecutable, gotArgs = executable, args
		return nil
	}}
	if err := plane.Start(context.Background(), home, active); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "platform", "generations", active.Generation, "workflow.exe")
	if gotExecutable != want || strings.Join(gotArgs, " ") != "serve-child --workflow-home "+home+" --generation "+active.Generation+" --bundle-digest "+strings.TrimPrefix(active.BundleDigest, "sha256:") {
		t.Fatalf("generation process = %q %#v", gotExecutable, gotArgs)
	}
}
