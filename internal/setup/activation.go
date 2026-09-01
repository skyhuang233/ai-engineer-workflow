package setup

import (
	"context"
	"errors"
	"time"

	"github.com/skyhuang233/workflow/internal/store"
)

// NativeService is intentionally limited to presence and first registration.
// Existing definitions are never compared, rewritten, or manually started by
// Setup; the Watch checkpoint is the behavior proof.
type NativeService interface {
	Present(context.Context) (bool, error)
	Register(context.Context) error
}

type WatchReader interface {
	RepositoryWatch(context.Context, string) (store.RepositoryWatch, error)
}

type ActivationResult struct {
	ServiceRegistered bool
	ReadyAfter        time.Time
}

// Activate registers a missing native service and waits only for the required
// Watch checkpoint. The caller owns the preceding platform/repository effects.
func Activate(ctx context.Context, service NativeService, watches WatchReader, watch store.RepositoryWatch, newlyInserted bool, timeout time.Duration, now func() time.Time) (ActivationResult, error) {
	if service == nil || watches == nil {
		return ActivationResult{}, errors.New("Repository Activation dependencies are incomplete")
	}
	if timeout <= 0 {
		return ActivationResult{}, errors.New("Repository Activation timeout must be positive")
	}
	if now == nil {
		now = time.Now
	}
	present, err := service.Present(ctx)
	if err != nil {
		return ActivationResult{}, err
	}
	result := ActivationResult{ReadyAfter: watch.RegisteredAt}
	if !present {
		if err := service.Register(ctx); err != nil {
			return result, err
		}
		result.ServiceRegistered = true
		result.ReadyAfter = now().UTC()
	}
	if !newlyInserted && !result.ServiceRegistered && !watch.LastSuccessfulPollAt.IsZero() && watch.LastSuccessfulPollAt.After(watch.RegisteredAt) {
		return result, nil
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		current, readErr := watches.RepositoryWatch(ctx, watch.Repository)
		if readErr != nil {
			return result, readErr
		}
		if current.LastSuccessfulPollAt.After(result.ReadyAfter) {
			return result, nil
		}
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case <-deadline.C:
			return result, errors.New("Control Plane did not complete a successful Issue poll before the readiness deadline")
		case <-ticker.C:
		}
	}
}
