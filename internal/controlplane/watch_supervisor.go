package controlplane

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/skyhuang233/workflow/internal/store"
)

// ObservedIssue is the immutable GitHub Issue handoff payload. The Watch
// neither interprets labels nor owns the task lifecycle that follows intake.
type ObservedIssue struct {
	ID      int64
	Number  int64
	Title   string
	Body    string
	State   string
	Created time.Time
	Updated time.Time
}

type RepositoryWatchStore interface {
	RepositoryWatches(context.Context) ([]store.RepositoryWatch, error)
	RecordRepositoryWatchPoll(context.Context, string, int64, time.Time) (store.RepositoryWatch, error)
}

type IssueObserver interface {
	IssuesAfter(context.Context, string, int64) ([]ObservedIssue, error)
}

// CodeTaskIntake is a separate capability. Implementations must make
// repository + immutable GitHub Issue ID idempotent and return the existing
// durable task on retries.
type CodeTaskIntake interface {
	AcceptIssue(context.Context, string, ObservedIssue) (taskReference string, err error)
}

// RepositoryWatchSupervisor continuously discovers Watches. A new Watch is
// polled immediately; periodic timing is only for subsequent observations.
// One Watch failure remains isolated from every other Watch.
type RepositoryWatchSupervisor struct {
	Store    RepositoryWatchStore
	Observer IssueObserver
	Intake   CodeTaskIntake
	Interval time.Duration
	Now      func() time.Time
	Report   func(string, error)
}

type watchLifetime struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func (s RepositoryWatchSupervisor) Run(ctx context.Context) error {
	if s.Store == nil || s.Observer == nil || s.Intake == nil {
		return errors.New("Repository Watch supervisor dependencies are incomplete")
	}
	interval := s.Interval
	if interval <= 0 {
		interval = time.Minute
	}
	now := s.Now
	if now == nil {
		now = time.Now
	}
	active := map[string]watchLifetime{}
	var mutex sync.Mutex
	start := func(watch store.RepositoryWatch) {
		if _, exists := active[watch.Repository]; exists {
			return
		}
		runCtx, cancel := context.WithCancel(ctx)
		lifetime := watchLifetime{cancel: cancel, done: make(chan struct{})}
		active[watch.Repository] = lifetime
		go func(initial store.RepositoryWatch, life watchLifetime) {
			defer close(life.done)
			// A failure is a runtime log concern. It intentionally leaves no
			// durable failure field on the Watch and retries on the interval.
			if err := s.poll(runCtx, initial, now); err != nil {
				s.report(initial.Repository, err)
			}
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-runCtx.Done():
					return
				case <-ticker.C:
					current, err := s.currentWatch(runCtx, initial.Repository)
					if err == nil {
						if err := s.poll(runCtx, current, now); err != nil {
							s.report(initial.Repository, err)
						}
					} else {
						s.report(initial.Repository, err)
					}
				}
			}
		}(watch, lifetime)
	}
	reconcile := func() error {
		watches, err := s.Store.RepositoryWatches(ctx)
		if err != nil {
			return err
		}
		present := make(map[string]struct{}, len(watches))
		mutex.Lock()
		defer mutex.Unlock()
		for _, watch := range watches {
			present[watch.Repository] = struct{}{}
			start(watch)
		}
		for repository, lifetime := range active {
			if _, exists := present[repository]; !exists {
				lifetime.cancel()
				delete(active, repository)
			}
		}
		return nil
	}
	if err := reconcile(); err != nil {
		return err
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			mutex.Lock()
			lifetimes := make([]watchLifetime, 0, len(active))
			for _, life := range active {
				life.cancel()
				lifetimes = append(lifetimes, life)
			}
			active = map[string]watchLifetime{}
			mutex.Unlock()
			for _, life := range lifetimes {
				<-life.done
			}
			return nil
		case <-ticker.C:
			if err := reconcile(); err != nil {
				return err
			}
		}
	}
}

func (s RepositoryWatchSupervisor) report(repository string, err error) {
	if s.Report != nil && err != nil {
		s.Report(repository, err)
	}
}

func (s RepositoryWatchSupervisor) currentWatch(ctx context.Context, repository string) (store.RepositoryWatch, error) {
	watches, err := s.Store.RepositoryWatches(ctx)
	if err != nil {
		return store.RepositoryWatch{}, err
	}
	for _, watch := range watches {
		if watch.Repository == repository {
			return watch, nil
		}
	}
	return store.RepositoryWatch{}, store.ErrNotFound
}

func (s RepositoryWatchSupervisor) poll(ctx context.Context, watch store.RepositoryWatch, now func() time.Time) error {
	issues, err := s.Observer.IssuesAfter(ctx, watch.Repository, watch.IssueCursor)
	if err != nil {
		return err
	}
	sort.Slice(issues, func(i, j int) bool { return issues[i].ID < issues[j].ID })
	cursor := watch.IssueCursor
	for _, issue := range issues {
		if issue.ID <= cursor {
			continue
		}
		if issue.ID <= 0 {
			return errors.New("GitHub returned an invalid Issue ID")
		}
		if _, err := s.Intake.AcceptIssue(ctx, watch.Repository, issue); err != nil {
			return err
		}
		cursor = issue.ID
	}
	_, err = s.Store.RecordRepositoryWatchPoll(ctx, watch.Repository, cursor, now().UTC())
	return err
}
