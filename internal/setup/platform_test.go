package setup

import (
	"context"
	"errors"
	"testing"
)

type probeFunc func(context.Context, string, string, string) error

func (f probeFunc) Verify(ctx context.Context, image, state, workspace string) error {
	return f(ctx, image, state, workspace)
}

type authenticationFunc func(context.Context) (string, bool, error)

func (f authenticationFunc) Ready(ctx context.Context) (string, bool, error) { return f(ctx) }

func TestPlatformPreparerProbesBeforeCheckingAuthentication(t *testing.T) {
	var calls []string
	preparer := PlatformPreparer{WorkerImage: "worker@sha256:abc", StateRoot: `C:\state`, WorkspaceRoot: `C:\workspace`, Probe: probeFunc(func(context.Context, string, string, string) error { calls = append(calls, "probe"); return nil }), Authentication: authenticationFunc(func(context.Context) (string, bool, error) {
		calls = append(calls, "auth")
		return "Codex login", true, nil
	})}
	mode, err := preparer.Prepare(context.Background())
	if err != nil || mode != "Codex login" || len(calls) != 2 || calls[0] != "probe" || calls[1] != "auth" {
		t.Fatalf("mode=%q calls=%q err=%v", mode, calls, err)
	}
}

func TestPlatformPreparerDoesNotCheckAuthenticationAfterProbeFailure(t *testing.T) {
	called := false
	preparer := PlatformPreparer{WorkerImage: "worker@sha256:abc", StateRoot: `C:\state`, WorkspaceRoot: `C:\workspace`, Probe: probeFunc(func(context.Context, string, string, string) error { return errors.New("mount failed") }), Authentication: authenticationFunc(func(context.Context) (string, bool, error) { called = true; return "", false, nil })}
	if _, err := preparer.Prepare(context.Background()); err == nil || called {
		t.Fatalf("err=%v called=%v", err, called)
	}
}

func TestPlatformPreparerReturnsMissingAuthenticationPrerequisite(t *testing.T) {
	preparer := PlatformPreparer{WorkerImage: "worker@sha256:abc", StateRoot: `C:\state`, WorkspaceRoot: `C:\workspace`, Probe: probeFunc(func(context.Context, string, string, string) error { return nil }), Authentication: authenticationFunc(func(context.Context) (string, bool, error) { return "API key or Codex login", false, nil })}
	_, err := preparer.Prepare(context.Background())
	var missing MissingWorkerAuthenticationError
	if err == nil || !errors.As(err, &missing) {
		t.Fatalf("err=%v", err)
	}
}
