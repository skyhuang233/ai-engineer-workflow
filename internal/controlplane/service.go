// Package controlplane owns the lifetime of the current-session Control Plane.
// It deliberately provides no supervision or restart loop.
package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"
)

type Identity struct {
	PID                      int       `json:"pid"`
	ProcessStartedAt         time.Time `json:"process_started_at"`
	PlatformVersion          string    `json:"platform_version"`
	ApprovedPlanDigestSHA256 string    `json:"approved_platform_bootstrap_plan_digest_sha256"`
}

type Health struct {
	Status   string   `json:"status"`
	Identity Identity `json:"identity"`
}

type Loop func(context.Context) error

// Service composes the health/control endpoint and the production loops that
// are active for exactly one foreground child lifetime.
type Service struct {
	Listener net.Listener
	Identity Identity
	Loops    []Loop
}

func (s Service) Run(parent context.Context) error {
	if s.Listener == nil {
		return errors.New("Control Plane listener is required")
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(Health{Status: "ready", Identity: s.Identity})
	})
	mux.HandleFunc("POST /shutdown", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]string{"status": "stopping"})
		go cancel()
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	results := make(chan error, len(s.Loops)+1)
	var workers sync.WaitGroup
	for _, loop := range s.Loops {
		if loop == nil {
			continue
		}
		workers.Add(1)
		go func(run Loop) {
			defer workers.Done()
			results <- run(ctx)
		}(loop)
	}
	go func() {
		err := server.Serve(s.Listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		results <- err
	}()

	var result error
	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.Canceled) {
			result = ctx.Err()
		}
	case result = <-results:
		cancel()
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	result = errors.Join(result, server.Shutdown(shutdownCtx))
	cancel()
	workers.Wait()
	return result
}
