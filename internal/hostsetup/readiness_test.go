package hostsetup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type commandExecutorFunc func(context.Context, []string) ([]byte, error)

func (f commandExecutorFunc) Run(ctx context.Context, args []string) ([]byte, error) {
	return f(ctx, args)
}

func TestVerifyDockerWorkerUsesSelectedMountsAndCleansMarkers(t *testing.T) {
	state, workspace := t.TempDir(), t.TempDir()
	calls := 0
	executor := commandExecutorFunc(func(_ context.Context, args []string) ([]byte, error) {
		calls++
		if calls == 1 {
			return []byte("linux/x86_64\n"), nil
		}
		joined := strings.Join(args, " ")
		if strings.HasPrefix(joined, "docker rm -f ") {
			if !strings.Contains(joined, "workflow-setup-docker-") {
				t.Fatalf("cleanup args=%#v", args)
			}
			return nil, nil
		}
		if !strings.Contains(joined, "source="+state) || !strings.Contains(joined, "source="+workspace) || !strings.Contains(joined, "host.docker.internal:host-gateway") || !strings.Contains(joined, "WORKFLOW_GATEWAY_PROBE_TOKEN") || !strings.Contains(joined, "worker@sha256:") || strings.Contains(joined, "readonly") {
			t.Fatalf("args=%#v", args)
		}
		if !strings.Contains(joined, "--name workflow-setup-docker-") || !strings.Contains(joined, "--label com.skyhuang233.workflow.setup-probe=true") {
			t.Fatalf("probe lacks unique ownership metadata: %#v", args)
		}
		if strings.Contains(joined, " --rm ") {
			t.Fatalf("probe cannot combine auto-removal with required explicit cleanup: %#v", args)
		}
		for _, root := range []string{state, workspace} {
			entries, readErr := os.ReadDir(root)
			if readErr != nil || len(entries) != 1 {
				t.Fatalf("probe root %s: entries=%v err=%v", root, entries, readErr)
			}
			if writeErr := os.WriteFile(filepath.Join(root, entries[0].Name(), "container-marker"), []byte("worker\n"), 0o600); writeErr != nil {
				t.Fatal(writeErr)
			}
		}
		return nil, nil
	})
	if err := VerifyDockerWorker(context.Background(), executor, "ghcr.io/owner/worker@sha256:"+strings.Repeat("a", 64), state, workspace); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("calls=%d, want info, run, and explicit rm", calls)
	}
	for _, root := range []string{state, workspace} {
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("probe residue in %s: %#v", filepath.Clean(root), entries)
		}
	}
}

func TestDockerWorkerVerifierAggregatesPrimaryAndEveryCleanupFailure(t *testing.T) {
	state, workspace := t.TempDir(), t.TempDir()
	calls := 0
	verifier := DockerWorkerVerifier{
		Executor: commandExecutorFunc(func(_ context.Context, _ []string) ([]byte, error) {
			calls++
			if calls == 1 {
				return []byte("linux/amd64"), nil
			}
			return []byte("probe failed"), errors.New("container failure")
		}),
		RemoveAll: func(path string) error { return errors.New("cleanup " + filepath.Base(path)) },
	}
	err := verifier.Verify(context.Background(), "ghcr.io/owner/worker@sha256:"+strings.Repeat("a", 64), state, workspace)
	if err == nil || !strings.Contains(err.Error(), "container failure") || !strings.Contains(err.Error(), "remove Docker readiness container") || strings.Count(err.Error(), "cleanup .setup-readiness-") != 2 {
		t.Fatalf("aggregated err=%v", err)
	}
}

func TestDockerWorkerVerifierTracksDeterministicCleanupResources(t *testing.T) {
	state, workspace := t.TempDir(), t.TempDir()
	var begun, completed []string
	verifier := DockerWorkerVerifier{
		ProbeID: "abcdef123456",
		BeginCleanup: func(kind, id, resource string) error {
			begun = append(begun, kind+"|"+id+"|"+resource)
			return nil
		},
		CompleteCleanup: func(id string) error {
			completed = append(completed, id)
			return nil
		},
		Executor: commandExecutorFunc(func(_ context.Context, args []string) ([]byte, error) {
			joined := strings.Join(args, " ")
			if strings.Contains(joined, " info ") {
				return []byte("linux/amd64"), nil
			}
			if strings.Contains(joined, " run ") {
				if !strings.Contains(joined, "--name workflow-setup-docker-abcdef123456") || !strings.Contains(joined, "setup-probe-id=abcdef123456") {
					t.Fatalf("nondeterministic probe command: %s", joined)
				}
				for _, root := range []string{state, workspace} {
					if err := os.WriteFile(filepath.Join(root, ".setup-readiness-abcdef123456", "container-marker"), []byte("worker\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			return nil, nil
		}),
	}
	if err := verifier.Verify(context.Background(), "worker@sha256:"+strings.Repeat("a", 64), state, workspace); err != nil {
		t.Fatal(err)
	}
	if len(begun) != 3 || len(completed) != 3 {
		t.Fatalf("cleanup tracking begun=%#v completed=%#v", begun, completed)
	}
	for _, want := range []string{"docker-state-probe", "docker-workspace-probe", "docker-container"} {
		if !strings.Contains(strings.Join(begun, "\n"), want) || !containsString(completed, want) {
			t.Fatalf("missing %s: begun=%#v completed=%#v", want, begun, completed)
		}
	}
}

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
