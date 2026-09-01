package setup

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// WorkerProbe is the capability-based platform gate. It proves the selected
// Worker can start and use the actual host state/workspace mounts; callers do
// not infer compatibility from Docker product or architecture strings.
type WorkerProbe interface {
	Verify(context.Context, string, string, string) error
}

// WorkerAuthentication reports readiness without exposing a credential. The
// selected authentication mode remains owned by its dedicated capability.
type WorkerAuthentication interface {
	Ready(context.Context) (mode string, ready bool, err error)
}

type PlatformPreparer struct {
	WorkerImage    string
	StateRoot      string
	WorkspaceRoot  string
	Probe          WorkerProbe
	Authentication WorkerAuthentication
}

type MissingWorkerAuthenticationError struct{ Modes string }

func (e MissingWorkerAuthenticationError) Error() string {
	if e.Modes == "" {
		return "Worker authentication is not ready"
	}
	return "Worker authentication is not ready; configure " + e.Modes
}

func (p PlatformPreparer) Prepare(ctx context.Context) (string, error) {
	if strings.TrimSpace(p.WorkerImage) == "" || !filepath.IsAbs(p.StateRoot) || !filepath.IsAbs(p.WorkspaceRoot) || p.Probe == nil || p.Authentication == nil {
		return "", errors.New("Platform Preparation dependencies are incomplete")
	}
	if err := p.Probe.Verify(ctx, p.WorkerImage, filepath.Clean(p.StateRoot), filepath.Clean(p.WorkspaceRoot)); err != nil {
		return "", fmt.Errorf("Worker Container Plumbing Probe: %w", err)
	}
	mode, ready, err := p.Authentication.Ready(ctx)
	if err != nil {
		return "", err
	}
	if !ready {
		return "", MissingWorkerAuthenticationError{Modes: mode}
	}
	return mode, nil
}
