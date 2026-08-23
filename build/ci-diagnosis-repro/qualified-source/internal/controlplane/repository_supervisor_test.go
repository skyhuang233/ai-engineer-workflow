package controlplane

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/skyhuang233/workflow/internal/store"
)

type supervisorSnapshot struct {
	mu         sync.Mutex
	admissions []store.RepositoryAdmission
	configs    []store.RepositoryRuntimeConfiguration
}

func (s *supervisorSnapshot) RepositoryAdmissions(context.Context) ([]store.RepositoryAdmission, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]store.RepositoryAdmission(nil), s.admissions...), nil
}
func (s *supervisorSnapshot) RepositoryRuntimeConfigurations(context.Context) ([]store.RepositoryRuntimeConfiguration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]store.RepositoryRuntimeConfiguration(nil), s.configs...), nil
}

type recordingRepositoryRunner struct{ started, stopped chan string }

func (r recordingRepositoryRunner) Run(ctx context.Context, config store.RepositoryRuntimeConfiguration) error {
	r.started <- config.Repository
	<-ctx.Done()
	r.stopped <- config.Repository
	return nil
}

func TestRepositorySupervisorRunsEligibleRepositoriesAndFencesSuspendedOnes(t *testing.T) {
	configured := store.RepositoryRuntimeConfiguration{Repository: "owner/admitted", DefaultBranch: "main", SourcePath: `C:\repo`, RootIssueNumber: 7, WorkspaceRoot: `C:\workspaces`, StateRoot: `C:\state`, CodexAuthFile: `C:\auth.json`, GitHubAPIURL: "https://api.github.com", PollInterval: time.Minute, WorkspaceRetention: time.Hour, MaxParallelRuns: 1, UpdatedAt: time.Now().UTC()}
	snapshot := &supervisorSnapshot{admissions: []store.RepositoryAdmission{{Repository: "owner/admitted", Eligible: true}, {Repository: "owner/suspended", Eligible: false}}, configs: []store.RepositoryRuntimeConfiguration{configured, {Repository: "owner/suspended", DefaultBranch: "main"}}}
	runner := recordingRepositoryRunner{started: make(chan string, 2), stopped: make(chan string, 2)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- (RepositorySupervisor{Store: snapshot, Runner: runner, Interval: 10 * time.Millisecond}).Run(ctx)
	}()
	select {
	case repo := <-runner.started:
		if repo != "owner/admitted" {
			t.Fatalf("started %s", repo)
		}
	case <-time.After(time.Second):
		t.Fatal("eligible repository did not start")
	}
	select {
	case repo := <-runner.started:
		t.Fatalf("suspended repository started: %s", repo)
	case <-time.After(30 * time.Millisecond):
	}
	snapshot.mu.Lock()
	snapshot.admissions[0].Eligible = false
	snapshot.mu.Unlock()
	select {
	case repo := <-runner.stopped:
		if repo != "owner/admitted" {
			t.Fatalf("stopped %s", repo)
		}
	case <-time.After(time.Second):
		t.Fatal("suspended repository was not cancelled")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRepositorySupervisorDoesNotScheduleEligibleRepositoryUntilRuntimeIsComplete(t *testing.T) {
	incomplete := store.RepositoryRuntimeConfiguration{Repository: "owner/repo", DefaultBranch: "main", SourcePath: `C:\repo`, GitHubAPIURL: "https://api.github.com", PollInterval: time.Minute, WorkspaceRetention: time.Hour, MaxParallelRuns: 1, UpdatedAt: time.Now().UTC()}
	snapshot := &supervisorSnapshot{admissions: []store.RepositoryAdmission{{Repository: "owner/repo", Eligible: true}}, configs: []store.RepositoryRuntimeConfiguration{incomplete}}
	runner := recordingRepositoryRunner{started: make(chan string, 1), stopped: make(chan string, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- (RepositorySupervisor{Store: snapshot, Runner: runner, Interval: 10 * time.Millisecond}).Run(ctx)
	}()
	select {
	case repository := <-runner.started:
		t.Fatalf("incomplete repository runtime started: %s", repository)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRepositorySupervisorCancelsAllRepositoriesWithLifecycle(t *testing.T) {
	config := store.RepositoryRuntimeConfiguration{Repository: "owner/repo", DefaultBranch: "main", SourcePath: `C:\repo`, RootIssueNumber: 7, WorkspaceRoot: `C:\workspaces`, StateRoot: `C:\state`, CodexAuthFile: `C:\auth.json`, GitHubAPIURL: "https://api.github.com", PollInterval: time.Minute, WorkspaceRetention: time.Hour, MaxParallelRuns: 1, UpdatedAt: time.Now().UTC()}
	snapshot := &supervisorSnapshot{admissions: []store.RepositoryAdmission{{Repository: "owner/repo", Eligible: true}}, configs: []store.RepositoryRuntimeConfiguration{config}}
	runner := recordingRepositoryRunner{started: make(chan string, 1), stopped: make(chan string, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- (RepositorySupervisor{Store: snapshot, Runner: runner, Interval: time.Hour}).Run(ctx) }()
	<-runner.started
	cancel()
	select {
	case <-runner.stopped:
	case <-time.After(time.Second):
		t.Fatal("repository lifetime was not cancelled")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRepositorySupervisorRestartsRepositoryWhenDurableConfigurationChanges(t *testing.T) {
	config := store.RepositoryRuntimeConfiguration{Repository: "owner/repo", DefaultBranch: "main", SourcePath: `C:\repo`, RootIssueNumber: 7, WorkspaceRoot: `C:\workspaces`, StateRoot: `C:\state`, CodexAuthFile: `C:\auth.json`, GitHubAPIURL: "https://api.github.com", PollInterval: time.Minute, WorkspaceRetention: time.Hour, MaxParallelRuns: 1, UpdatedAt: time.Now().UTC()}
	snapshot := &supervisorSnapshot{admissions: []store.RepositoryAdmission{{Repository: "owner/repo", Eligible: true}}, configs: []store.RepositoryRuntimeConfiguration{config}}
	runner := recordingRepositoryRunner{started: make(chan string, 2), stopped: make(chan string, 2)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- (RepositorySupervisor{Store: snapshot, Runner: runner, Interval: 10 * time.Millisecond}).Run(ctx)
	}()
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("repository did not start")
	}
	snapshot.mu.Lock()
	snapshot.configs[0].RootIssueNumber = 8
	snapshot.configs[0].UpdatedAt = snapshot.configs[0].UpdatedAt.Add(time.Second)
	snapshot.mu.Unlock()
	select {
	case <-runner.stopped:
	case <-time.After(time.Second):
		t.Fatal("old repository lifetime was not cancelled")
	}
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("repository did not restart with updated configuration")
	}
	cancel()
	select {
	case <-runner.stopped:
	case <-time.After(time.Second):
		t.Fatal("updated repository lifetime was not cancelled")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
