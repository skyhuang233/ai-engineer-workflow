package launcher

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skyhuang233/workflow/internal/platformrelease"
)

type fixedDockerInspector struct {
	version string
	err     error
}

func (f fixedDockerInspector) DockerVersion(context.Context) (string, error) { return f.version, f.err }

type recordingDependencies struct {
	version string
	docker  []DockerCapabilityTarget
	worker  []WorkerImageCapabilityTarget
}

func (d *recordingDependencies) DockerVersion(context.Context) (string, error) { return d.version, nil }
func (d *recordingDependencies) EnsureDocker(_ context.Context, target DockerCapabilityTarget) error {
	d.docker = append(d.docker, target)
	return nil
}
func (d *recordingDependencies) EnsureWorkerImage(_ context.Context, target WorkerImageCapabilityTarget) error {
	d.worker = append(d.worker, target)
	return nil
}

func TestDependencyCapabilitiesBindExactObservedDockerAndWorkerImage(t *testing.T) {
	bundle := t.TempDir()
	writeTestBundle(t, bundle, map[string]string{"platform/workflow.exe": "cli", "setup/workflow-setup.exe": "setup", "skills/agent-workflow/SKILL.md": "skill", "repository-contract/repository.json": "contract"})
	request := Request{SchemaVersion: ProtocolVersion, Operation: Inspect, Purpose: PurposeTargetState, WorkflowHome: filepath.Join(t.TempDir(), "home"), TargetVersion: "0.0.1", BundleDigest: "sha256:" + strings.Repeat("a", 64), GitHubOwner: "owner"}

	for _, test := range []struct {
		name    string
		version string
		want    string
	}{
		{name: "exact reuse", version: "4.86.0", want: dockerActionReuse},
		{name: "missing install", version: "", want: dockerActionInstall},
		{name: "version mismatch upgrade", version: "4.85.0", want: dockerActionUpgrade},
	} {
		t.Run(test.name, func(t *testing.T) {
			engine := Engine{BundleRoot: bundle, DependencyInspector: fixedDockerInspector{version: test.version}}
			capabilities, err := engine.requiredCapabilities(context.Background(), request, nil)
			if err != nil {
				t.Fatal(err)
			}
			docker, err := dockerCapability(capabilities)
			if err != nil || docker.Action != test.want || docker.RequiredVersion != "4.86.0" {
				t.Fatalf("docker=%#v err=%v", docker, err)
			}
			if test.want == dockerActionReuse && (docker.ObservedVersion != "4.86.0" || docker.InstallerURL != "" || docker.InstallerSHA256 != "") {
				t.Fatalf("reuse target=%#v", docker)
			}
			if test.want != dockerActionReuse && (docker.InstallerURL == "" || docker.InstallerSHA256 != strings.Repeat("b", 64)) {
				t.Fatalf("host-changing target=%#v", docker)
			}
			worker, err := workerImageCapability(capabilities)
			if err != nil || worker.Image != "ghcr.io/skyhuang233/workflow-worker@sha256:"+strings.Repeat("a", 64) {
				t.Fatalf("worker=%#v err=%v", worker, err)
			}
		})
	}
}

func TestDependencyTargetChangeRequiresNewConsentAndPrepareUsesAcceptedTarget(t *testing.T) {
	bundle := t.TempDir()
	writeTestBundle(t, bundle, map[string]string{"platform/workflow.exe": "cli", "setup/workflow-setup.exe": "setup", "skills/agent-workflow/SKILL.md": "skill", "repository-contract/repository.json": "contract"})
	home := filepath.Join(t.TempDir(), "home")
	digest := "sha256:" + strings.Repeat("c", 64)
	dependencies := &recordingDependencies{version: "4.86.0"}
	engine := Engine{BundleRoot: bundle, DependencyInspector: dependencies}
	request := Request{SchemaVersion: ProtocolVersion, Operation: Apply, WorkflowHome: home, TargetVersion: "0.0.1", BundleDigest: digest, GitHubOwner: "owner"}
	request.AcceptedCapabilities = requiredCapabilities(t, engine, request)
	if got, err := engine.Apply(context.Background(), request); err != nil || got.Status != "ready" {
		t.Fatalf("apply=%#v,%v", got, err)
	}
	active, err := ReadActive(home)
	if err != nil {
		t.Fatal(err)
	}
	consent, err := readConsent(home, active.ConsentID)
	if err != nil {
		t.Fatal(err)
	}
	compatibility, err := engine.bundleCompatibility()
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := &BundleLifecycle{Compatibility: compatibility, Dependencies: dependencies}
	if err := lifecycle.Prepare(context.Background(), request, consent); err != nil {
		t.Fatal(err)
	}
	if len(dependencies.docker) != 1 || dependencies.docker[0].Action != dockerActionReuse || len(dependencies.worker) != 1 || dependencies.worker[0].Image != compatibility.WorkerImage {
		t.Fatalf("Prepare targets: docker=%#v worker=%#v", dependencies.docker, dependencies.worker)
	}

	// A newly observed incompatible version produces a distinct consent target;
	// the original exact Consent cannot authorize the upgrade.
	engine.DependencyInspector = fixedDockerInspector{version: "4.85.0"}
	inspect := Request{SchemaVersion: ProtocolVersion, Operation: Inspect, Purpose: PurposeTargetState, WorkflowHome: home, TargetVersion: "0.0.1", BundleDigest: digest, GitHubOwner: "owner"}
	if got, err := engine.Inspect(context.Background(), inspect); err != nil || got.Status != "consent_required" {
		t.Fatalf("Docker change inspect=%#v,%v", got, err)
	}

	// The immutable image is likewise part of the exact consent comparison.
	changedBundle := t.TempDir()
	writeTestBundle(t, changedBundle, map[string]string{"platform/workflow.exe": "cli", "setup/workflow-setup.exe": "setup", "skills/agent-workflow/SKILL.md": "skill", "repository-contract/repository.json": "contract"})
	rewriteWorkerImage(t, changedBundle, "ghcr.io/skyhuang233/workflow-worker@sha256:"+strings.Repeat("d", 64))
	engine.BundleRoot, engine.DependencyInspector = changedBundle, fixedDockerInspector{version: "4.86.0"}
	if got, err := engine.Inspect(context.Background(), inspect); err != nil || got.Status != "consent_required" {
		t.Fatalf("image change inspect=%#v,%v", got, err)
	}
}

func TestDependencyCapabilitiesRejectGenericOrEmptyTargets(t *testing.T) {
	bundle := t.TempDir()
	writeTestBundle(t, bundle, map[string]string{"platform/workflow.exe": "cli", "setup/workflow-setup.exe": "setup", "skills/agent-workflow/SKILL.md": "skill", "repository-contract/repository.json": "contract"})
	engine := Engine{BundleRoot: bundle, DependencyInspector: fixedDockerInspector{version: "4.86.0"}}
	home := filepath.Join(t.TempDir(), "home")
	request := Request{SchemaVersion: ProtocolVersion, Operation: Apply, WorkflowHome: home, TargetVersion: "0.0.1", BundleDigest: "sha256:" + strings.Repeat("e", 64), GitHubOwner: "owner"}
	request.AcceptedCapabilities = requiredCapabilities(t, engine, request)
	for i := range request.AcceptedCapabilities {
		if request.AcceptedCapabilities[i].Name == "manage_docker_desktop" {
			request.AcceptedCapabilities[i].Value = "bundle-declared Docker Desktop"
		}
	}
	if got, err := engine.Apply(context.Background(), request); err != nil || got.Status != "consent_required" {
		t.Fatalf("generic Docker target apply=%#v,%v", got, err)
	}
	if _, err := os.Stat(home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("generic target created Home: %v", err)
	}
	request.AcceptedCapabilities = requiredCapabilities(t, engine, request)
	for i := range request.AcceptedCapabilities {
		if request.AcceptedCapabilities[i].Name == "pull_worker_image" {
			request.AcceptedCapabilities[i].Value = ""
		}
	}
	if got, err := engine.Apply(context.Background(), request); err != nil || got.Status != "blocked" {
		t.Fatalf("empty worker target apply=%#v,%v", got, err)
	}
}

func rewriteWorkerImage(t *testing.T, bundle, image string) {
	t.Helper()
	path := filepath.Join(bundle, "platform-release.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var manifest platformrelease.BundleManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Compatibility.WorkerImage = image
	raw, err = manifest.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}
