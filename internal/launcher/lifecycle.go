package launcher

// This file is deliberately a small boundary around external Windows state.
// Engine owns activation ordering; these adapters make Docker and the control
// plane replaceable in tests and keep shell/process details out of that state
// machine.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/skyhuang233/workflow/internal/controlplane"
	"github.com/skyhuang233/workflow/internal/hostsetup"
	"github.com/skyhuang233/workflow/internal/platformrelease"
	"github.com/skyhuang233/workflow/internal/workflowhome"
)

type DependencyAdapter interface {
	DockerVersion(context.Context) (string, error)
	EnsureDocker(context.Context, DockerCapabilityTarget) error
	EnsureWorkerImage(context.Context, WorkerImageCapabilityTarget) error
}

type ControlPlaneAdapter interface {
	Stop(context.Context, string, Active) error
	Start(context.Context, string, Active) error
	Ready(context.Context, string, Active) error
}

type BundleLifecycle struct {
	Compatibility platformrelease.Compatibility
	Dependencies  DependencyAdapter
	ControlPlane  ControlPlaneAdapter
}

func NewBundleLifecycle(bundleRoot string) (*BundleLifecycle, error) {
	raw, err := os.ReadFile(filepath.Join(bundleRoot, "platform-release.json"))
	if err != nil {
		return nil, fmt.Errorf("read Bundle manifest: %w", err)
	}
	var manifest platformrelease.BundleManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, fmt.Errorf("decode Bundle manifest: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	return &BundleLifecycle{Compatibility: manifest.Compatibility, Dependencies: WindowsDependencies{}, ControlPlane: WindowsControlPlane{}}, nil
}

func (l *BundleLifecycle) DockerVersion(ctx context.Context) (string, error) {
	if l == nil || l.Dependencies == nil {
		return "", errors.New("Windows dependency adapter is required")
	}
	return l.Dependencies.DockerVersion(ctx)
}

func (l *BundleLifecycle) Prepare(ctx context.Context, _ Request, consent Consent) error {
	if l == nil || l.Dependencies == nil {
		return errors.New("Windows dependency adapter is required")
	}
	docker, err := dockerCapability(consent.Capabilities)
	if err != nil {
		return err
	}
	if err := validateDockerCapability(docker, l.Compatibility); err != nil {
		return err
	}
	worker, err := workerImageCapability(consent.Capabilities)
	if err != nil {
		return err
	}
	if worker.Image != l.Compatibility.WorkerImage {
		return errors.New("worker image consent differs from Bundle manifest")
	}
	if err := l.Dependencies.EnsureDocker(ctx, docker); err != nil {
		return err
	}
	return l.Dependencies.EnsureWorkerImage(ctx, worker)
}
func (l *BundleLifecycle) Stop(ctx context.Context, home string, active Active) error {
	if l == nil || l.ControlPlane == nil {
		return errors.New("Control Plane adapter is required")
	}
	return l.ControlPlane.Stop(ctx, home, active)
}
func (l *BundleLifecycle) Start(ctx context.Context, home string, active Active) error {
	if l == nil || l.ControlPlane == nil {
		return errors.New("Control Plane adapter is required")
	}
	return l.ControlPlane.Start(ctx, home, active)
}
func (l *BundleLifecycle) Ready(ctx context.Context, home string, active Active) error {
	if l == nil || l.ControlPlane == nil {
		return errors.New("Control Plane adapter is required")
	}
	return l.ControlPlane.Ready(ctx, home, active)
}
func (l *BundleLifecycle) Restart(ctx context.Context, home string, active Active) error {
	if err := l.Start(ctx, home, active); err != nil {
		return err
	}
	return l.Ready(ctx, home, active)
}

// CommandRunner is the process seam for Windows dependency management.
type CommandRunner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}
type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// WindowsDependencies treats the Docker Desktop registry DisplayVersion as
// the consent-bound product version. Docker Engine versions are intentionally
// not comparable to Bundle Docker Desktop versions. The Runner remains the
// worker-image seam; Docker Desktop itself is delegated to the existing host
// adapter so its registry readback, elevated install, start, and bounded
// daemon readiness contract stay in one place.
type WindowsDependencies struct {
	Runner        CommandRunner
	DockerHost    hostsetup.DockerDesktopHost
	TemporaryRoot string
	DockerTimeout time.Duration
}

func (d WindowsDependencies) runner() CommandRunner {
	if d.Runner != nil {
		return d.Runner
	}
	return execRunner{}
}

func (d WindowsDependencies) dockerHost() hostsetup.DockerDesktopHost {
	if d.DockerHost != nil {
		return d.DockerHost
	}
	return hostsetup.WindowsDockerDesktopHost{}
}

func (d WindowsDependencies) dockerTemporaryRoot() string {
	if strings.TrimSpace(d.TemporaryRoot) != "" {
		return d.TemporaryRoot
	}
	return filepath.Join(os.TempDir(), "workflow-docker-desktop")
}

func (d WindowsDependencies) DockerVersion(ctx context.Context) (string, error) {
	version, err := d.dockerHost().InstalledVersion(ctx)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(version), nil
}

func (d WindowsDependencies) EnsureDocker(ctx context.Context, target DockerCapabilityTarget) error {
	if err := target.valid(); err != nil {
		return err
	}
	if target.Action == dockerActionReuse {
		observed, err := d.DockerVersion(ctx)
		if err != nil {
			return err
		}
		if observed != target.ObservedVersion || observed != target.RequiredVersion {
			return errors.New("Docker Desktop version no longer matches accepted reuse target")
		}
	}
	contract := hostsetup.DockerDesktopContract{Version: target.RequiredVersion, InstallerURL: target.InstallerURL, WindowsAMD64SHA256: target.InstallerSHA256}
	if err := hostsetup.EnsureDockerDesktop(ctx, contract, d.dockerHost(), d.dockerTemporaryRoot(), d.DockerTimeout); err != nil {
		return fmt.Errorf("ensure Docker Desktop: %w", err)
	}
	return nil
}
func (d WindowsDependencies) EnsureWorkerImage(ctx context.Context, target WorkerImageCapabilityTarget) error {
	if err := target.valid(); err != nil {
		return err
	}
	if _, err := d.runner().Run(ctx, "docker", "pull", target.Image); err != nil {
		return fmt.Errorf("pull exact worker image: %w", err)
	}
	if _, err := d.runner().Run(ctx, "docker", "image", "inspect", target.Image); err != nil {
		return fmt.Errorf("verify exact worker image: %w", err)
	}
	return nil
}

// WindowsControlPlane starts the versioned executable from the selected
// generation. It accepts readiness only after a live HTTP response identifies
// that same generation and version; a file or process existence check alone is
// never an activation authority.
type WindowsControlPlane struct {
	StartProcess func(context.Context, string, ...string) error
}

func (c WindowsControlPlane) Start(ctx context.Context, home string, active Active) error {
	executable := filepath.Join(home, "platform", "generations", active.Generation, "workflow.exe")
	args := []string{"serve-child", "--workflow-home", home, "--generation", active.Generation, "--bundle-digest", strings.TrimPrefix(active.BundleDigest, "sha256:")}
	if c.StartProcess != nil {
		return c.StartProcess(ctx, executable, args...)
	}
	layout, err := workflowhome.Resolve(home)
	if err != nil {
		return err
	}
	_, err = controlplane.Start(ctx, controlplane.StartOptions{Layout: layout, Executable: executable, PlatformVersion: active.Version, Generation: active.Generation, BundleDigest: strings.TrimPrefix(active.BundleDigest, "sha256:")})
	return err
}
func (c WindowsControlPlane) Stop(_ context.Context, home string, active Active) error {
	layout, err := workflowhome.Resolve(home)
	if err != nil {
		return err
	}
	record, err := controlplane.ReadRuntimeRecord(layout)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if record.Generation != active.Generation || record.BundleDigest != strings.TrimPrefix(active.BundleDigest, "sha256:") {
		return errors.New("Control Plane runtime identity is invalid")
	}
	return controlplane.Stop(context.Background(), record, controlplane.Inspector{})
}
func (c WindowsControlPlane) Ready(ctx context.Context, home string, active Active) error {
	layout, err := workflowhome.Resolve(home)
	if err != nil {
		return err
	}
	record, err := controlplane.WaitReady(ctx, layout, controlplane.Inspector{})
	if err != nil {
		return err
	}
	if record.Generation != active.Generation || record.PlatformVersion != active.Version || record.BundleDigest != strings.TrimPrefix(active.BundleDigest, "sha256:") {
		return errors.New("Control Plane ready identity differs from active generation")
	}
	return nil
}

func runtimeHealthURL(address string) string {
	return "http://" + strings.TrimPrefix(address, "http://") + "/health"
}
