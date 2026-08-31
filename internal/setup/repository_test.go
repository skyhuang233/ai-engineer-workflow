package setup

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeLocal struct {
	resolved LocalRepository
	missing  bool
	state    PublicationState
	calls    []string
}

func (f *fakeLocal) Resolve(context.Context) (LocalRepository, error) {
	f.calls = append(f.calls, "resolve")
	if f.missing {
		return LocalRepository{}, ErrLocalRepositoryNotFound
	}
	return f.resolved, nil
}
func (f *fakeLocal) Initialize(context.Context) (LocalRepository, error) {
	f.calls = append(f.calls, "init")
	f.missing = false
	return f.resolved, nil
}
func (f *fakeLocal) CreateEmptyBaseline(_ context.Context, value LocalRepository) (LocalRepository, error) {
	f.calls = append(f.calls, "baseline")
	value.HasCommit = true
	f.resolved = value
	return value, nil
}
func (f *fakeLocal) PublicationState(context.Context, LocalRepository, RepositoryAddress) (PublicationState, error) {
	f.calls = append(f.calls, "publication")
	return f.state, nil
}
func (f *fakeLocal) PublishCurrentBranch(context.Context, LocalRepository, RepositoryAddress) error {
	f.calls = append(f.calls, "push")
	return nil
}

type fakeGitHub struct {
	repository GitHubRepository
	boundary   int64
	calls      []string
}

func (f *fakeGitHub) Repository(context.Context, RepositoryAddress) (GitHubRepository, error) {
	f.calls = append(f.calls, "repository")
	return f.repository, nil
}
func (f *fakeGitHub) CreatePrivateRepository(context.Context, RepositoryAddress) error {
	f.calls = append(f.calls, "create")
	f.repository.Exists = true
	return nil
}
func (f *fakeGitHub) SetDefaultBranch(_ context.Context, _ RepositoryAddress, branch string) error {
	f.calls = append(f.calls, "default:"+branch)
	f.repository.DefaultBranch = branch
	return nil
}
func (f *fakeGitHub) EnableIssues(context.Context, RepositoryAddress) error {
	f.calls = append(f.calls, "issues")
	return nil
}
func (f *fakeGitHub) LatestIssueID(context.Context, RepositoryAddress) (int64, error) {
	f.calls = append(f.calls, "boundary")
	return f.boundary, nil
}

type fakeWatches struct {
	repository string
	boundary   int64
	registered time.Time
	inserted   bool
}

func (f *fakeWatches) RecordWatch(_ context.Context, repository string, registered time.Time, boundary int64) (time.Time, bool, error) {
	f.repository, f.boundary, f.registered = repository, boundary, registered
	return registered, f.inserted, nil
}

func TestRepositoryReconcilerMovesForwardAndCreatesOneWatch(t *testing.T) {
	local := &fakeLocal{resolved: LocalRepository{Root: "/work", Branch: "main"}, missing: true, state: PublicationCanFastForward}
	github := &fakeGitHub{repository: GitHubRepository{IssuesEnabled: false}, boundary: 99}
	watches := &fakeWatches{inserted: true}
	now := time.Date(2026, 8, 31, 1, 2, 3, 0, time.UTC)
	result, err := (RepositoryReconciler{Local: local, GitHub: github, Watches: watches, Now: func() time.Time { return now }}).Reconcile(context.Background(), RepositoryAddress{Owner: "owner", Name: "repository"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Initialized || !result.BaselineMade || !result.Created || !result.Published || !result.Defaulted || !result.IssuesEnabled || !result.WatchInserted {
		t.Fatalf("unexpected result: %+v", result)
	}
	if watches.repository != "owner/repository" || watches.boundary != 99 || !watches.registered.Equal(now) {
		t.Fatalf("watch = %+v", watches)
	}
}

func TestRepositoryReconcilerStopsBeforeWatchOnHistoryConflict(t *testing.T) {
	local := &fakeLocal{resolved: LocalRepository{Root: "/work", Branch: "main", HasCommit: true}, state: PublicationDiverged}
	watches := &fakeWatches{}
	_, err := (RepositoryReconciler{Local: local, GitHub: &fakeGitHub{repository: GitHubRepository{Exists: true, IssuesEnabled: true}}, Watches: watches}).Reconcile(context.Background(), RepositoryAddress{Owner: "owner", Name: "repository"})
	if !errors.Is(err, ErrRepositoryHistoryConflict) {
		t.Fatalf("error = %v", err)
	}
	if watches.repository != "" {
		t.Fatalf("history conflict wrote watch: %+v", watches)
	}
}
