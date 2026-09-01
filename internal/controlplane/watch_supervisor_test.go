package controlplane

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/skyhuang233/workflow/internal/store"
)

type memoryWatchStore struct {
	mutex   sync.Mutex
	watches []store.RepositoryWatch
}

func (s *memoryWatchStore) RepositoryWatches(context.Context) ([]store.RepositoryWatch, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return append([]store.RepositoryWatch(nil), s.watches...), nil
}
func (s *memoryWatchStore) RecordRepositoryWatchPoll(_ context.Context, repository string, cursor int64, successful time.Time) (store.RepositoryWatch, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	for i := range s.watches {
		if s.watches[i].Repository == repository {
			if cursor > s.watches[i].IssueCursor {
				s.watches[i].IssueCursor = cursor
			}
			s.watches[i].LastSuccessfulPollAt = successful
			return s.watches[i], nil
		}
	}
	return store.RepositoryWatch{}, store.ErrNotFound
}

type memoryObserver struct{ issues []ObservedIssue }

func (o memoryObserver) IssuesAfter(_ context.Context, _ string, cursor int64) ([]ObservedIssue, error) {
	var result []ObservedIssue
	for _, issue := range o.issues {
		if issue.ID > cursor {
			result = append(result, issue)
		}
	}
	return result, nil
}

type recordingIntake struct {
	mutex  sync.Mutex
	IDs    []int64
	failID int64
}

func (i *recordingIntake) AcceptIssue(_ context.Context, _ string, issue ObservedIssue) (string, error) {
	i.mutex.Lock()
	defer i.mutex.Unlock()
	if issue.ID == i.failID {
		return "", context.DeadlineExceeded
	}
	i.IDs = append(i.IDs, issue.ID)
	return "task", nil
}

func TestRepositoryWatchPollAcceptsBeforeAdvancingCursor(t *testing.T) {
	registered := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	database := &memoryWatchStore{watches: []store.RepositoryWatch{{Repository: "owner/repository", RegisteredAt: registered, IssueCursor: 10}}}
	intake := &recordingIntake{failID: 12}
	supervisor := RepositoryWatchSupervisor{Store: database, Observer: memoryObserver{issues: []ObservedIssue{{ID: 12}, {ID: 11}}}, Intake: intake, Now: func() time.Time { return registered.Add(time.Minute) }}
	watch := database.watches[0]
	if err := supervisor.poll(context.Background(), watch, supervisor.Now); err == nil {
		t.Fatal("acceptance failure did not stop poll")
	}
	if len(intake.IDs) != 1 || intake.IDs[0] != 11 || database.watches[0].IssueCursor != 10 {
		t.Fatalf("intake=%v watch=%+v", intake.IDs, database.watches[0])
	}

	intake.failID = 0
	if err := supervisor.poll(context.Background(), database.watches[0], supervisor.Now); err != nil {
		t.Fatal(err)
	}
	if database.watches[0].IssueCursor != 12 || database.watches[0].LastSuccessfulPollAt.IsZero() {
		t.Fatalf("watch=%+v", database.watches[0])
	}
}

func TestRepositoryWatchPollCheckpointsAnEmptySuccessfulResponse(t *testing.T) {
	registered := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	database := &memoryWatchStore{watches: []store.RepositoryWatch{{Repository: "owner/repository", RegisteredAt: registered, IssueCursor: 10}}}
	supervisor := RepositoryWatchSupervisor{Store: database, Observer: memoryObserver{}, Intake: &recordingIntake{}, Now: func() time.Time { return registered.Add(time.Minute) }}
	if err := supervisor.poll(context.Background(), database.watches[0], supervisor.Now); err != nil {
		t.Fatal(err)
	}
	if !database.watches[0].LastSuccessfulPollAt.Equal(registered.Add(time.Minute)) {
		t.Fatalf("watch=%+v", database.watches[0])
	}
}
