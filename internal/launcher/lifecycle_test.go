package launcher

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

type recordingRunner struct{ calls [][]string }

func (r *recordingRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	if name == "docker" && len(args) > 0 && args[0] == "version" {
		return []byte("4.86.0\n"), nil
	}
	return []byte("ok"), nil
}

func TestWindowsDependenciesReusesDockerAndPullsExactImage(t *testing.T) {
	runner := &recordingRunner{}
	dependencies := WindowsDependencies{Runner: runner}
	image := "ghcr.io/owner/worker@sha256:" + strings.Repeat("a", 64)
	docker := DockerCapabilityTarget{Action: dockerActionReuse, RequiredVersion: "4.86.0", ObservedVersion: "4.86.0", HostImpact: "reuse verified Docker Desktop"}
	if err := dependencies.EnsureDocker(context.Background(), docker); err != nil {
		t.Fatal(err)
	}
	if err := dependencies.EnsureWorkerImage(context.Background(), WorkerImageCapabilityTarget{Image: image}); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 3 || runner.calls[0][0] != "docker" || runner.calls[1][1] != "pull" || runner.calls[1][2] != image || runner.calls[2][1] != "image" {
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
