package controlplane

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/skyhuang233/workflow/internal/store"
)

type RepositoryRuntimeStore interface {
	RepositoryAdmissions(context.Context) ([]store.RepositoryAdmission, error)
	RepositoryRuntimeConfigurations(context.Context) ([]store.RepositoryRuntimeConfiguration, error)
}

type RepositoryRunner interface {
	Run(context.Context, store.RepositoryRuntimeConfiguration) error
}
type RepositoryRunnerFunc func(context.Context, store.RepositoryRuntimeConfiguration) error

func (f RepositoryRunnerFunc) Run(ctx context.Context, config store.RepositoryRuntimeConfiguration) error {
	return f(ctx, config)
}

// RepositorySupervisor owns one cancellable business-loop lifetime per
// admitted repository. A repository failure is isolated and never terminates
// another repository or the Control Plane.
type RepositorySupervisor struct {
	Store    RepositoryRuntimeStore
	Runner   RepositoryRunner
	Interval time.Duration
}

type repositoryLifetime struct {
	cancel context.CancelFunc
	done   chan struct{}
	config store.RepositoryRuntimeConfiguration
}

func (s RepositorySupervisor) Run(ctx context.Context) error {
	if s.Store == nil || s.Runner == nil {
		return errors.New("repository supervisor dependencies are incomplete")
	}
	if s.Interval <= 0 {
		s.Interval = time.Second
	}
	active := map[string]repositoryLifetime{}
	var mu sync.Mutex
	reconcile := func() error {
		admissions, err := s.Store.RepositoryAdmissions(ctx)
		if err != nil {
			return err
		}
		configs, err := s.Store.RepositoryRuntimeConfigurations(ctx)
		if err != nil {
			return err
		}
		eligible := map[string]bool{}
		for _, value := range admissions {
			eligible[value.Repository] = value.Eligible
		}
		desired := map[string]store.RepositoryRuntimeConfiguration{}
		for _, config := range configs {
			if eligible[config.Repository] && config.Ready() == nil {
				desired[config.Repository] = config
			}
		}
		mu.Lock()
		defer mu.Unlock()
		for repository, lifetime := range active {
			config, keep := desired[repository]
			if keep && config == lifetime.config {
				continue
			}
			lifetime.cancel()
		}
		for repository, config := range desired {
			if _, exists := active[repository]; exists {
				continue
			}
			runCtx, cancel := context.WithCancel(ctx)
			lifetime := repositoryLifetime{cancel: cancel, done: make(chan struct{}), config: config}
			active[repository] = lifetime
			go func(repository string, config store.RepositoryRuntimeConfiguration, lifetime repositoryLifetime) {
				defer close(lifetime.done)
				_ = s.Runner.Run(runCtx, config)
				mu.Lock()
				if current, ok := active[repository]; ok && current.done == lifetime.done {
					delete(active, repository)
				}
				mu.Unlock()
			}(repository, config, lifetime)
		}
		return nil
	}
	if err := reconcile(); err != nil {
		return err
	}
	ticker := time.NewTicker(s.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			mu.Lock()
			lifetimes := make([]repositoryLifetime, 0, len(active))
			for _, lifetime := range active {
				lifetime.cancel()
				lifetimes = append(lifetimes, lifetime)
			}
			active = map[string]repositoryLifetime{}
			mu.Unlock()
			for _, lifetime := range lifetimes {
				<-lifetime.done
			}
			return nil
		case <-ticker.C:
			if err := reconcile(); err != nil {
				return err
			}
		}
	}
}
