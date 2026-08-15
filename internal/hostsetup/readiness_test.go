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
		if !strings.Contains(joined, "source="+state) || !strings.Contains(joined, "source="+workspace) || !strings.Contains(joined, "host.docker.internal:host-gateway") || !strings.Contains(joined, "WORKFLOW_GATEWAY_PROBE_TOKEN") || !strings.Contains(joined, "worker@sha256:") || strings.Contains(joined, "readonly") {
			t.Fatalf("args=%#v", args)
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
	if err == nil || !strings.Contains(err.Error(), "container failure") || strings.Count(err.Error(), "cleanup .setup-readiness-") != 2 {
		t.Fatalf("aggregated err=%v", err)
	}
}
