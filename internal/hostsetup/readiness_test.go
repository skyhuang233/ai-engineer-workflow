package hostsetup

import (
	"context"
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
		if !strings.Contains(joined, "source="+state) || !strings.Contains(joined, "source="+workspace) || !strings.Contains(joined, "getent hosts github.com") || !strings.Contains(joined, "worker@sha256:") {
			t.Fatalf("args=%#v", args)
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
