package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestRepositoryWatchIsInsertedOnceAndTracksSuccessfulPolls(t *testing.T) {
	database := newRepositoryWatchStore(t)
	ctx := context.Background()
	registered := time.Date(2026, 8, 31, 1, 2, 3, 0, time.UTC)
	watch, inserted, err := database.RecordRepositoryWatch(ctx, RepositoryWatch{Repository: "owner/repository", RegisteredAt: registered, IssueCursor: 41})
	if err != nil || !inserted {
		t.Fatalf("insert watch: inserted=%v err=%v", inserted, err)
	}
	if watch.IssueCursor != 41 || !watch.LastSuccessfulPollAt.IsZero() {
		t.Fatalf("unexpected initial watch: %+v", watch)
	}

	watch, inserted, err = database.RecordRepositoryWatch(ctx, RepositoryWatch{Repository: "owner/repository", RegisteredAt: registered.Add(time.Hour), IssueCursor: 99})
	if err != nil || inserted || watch.IssueCursor != 41 || !watch.RegisteredAt.Equal(registered) {
		t.Fatalf("reuse watch: %+v inserted=%v err=%v", watch, inserted, err)
	}

	poll := registered.Add(time.Minute)
	watch, err = database.RecordRepositoryWatchPoll(ctx, "owner/repository", 57, poll)
	if err != nil || watch.IssueCursor != 57 || !watch.LastSuccessfulPollAt.Equal(poll) {
		t.Fatalf("record poll: %+v err=%v", watch, err)
	}
	watch, err = database.RecordRepositoryWatchPoll(ctx, "owner/repository", 44, poll.Add(time.Minute))
	if err != nil || watch.IssueCursor != 57 || !watch.LastSuccessfulPollAt.Equal(poll.Add(time.Minute)) {
		t.Fatalf("monotonic cursor: %+v err=%v", watch, err)
	}
}

func TestRepositoryWatchRejectsInvalidIdentityAndMissingWatch(t *testing.T) {
	database := newRepositoryWatchStore(t)
	_, _, err := database.RecordRepositoryWatch(context.Background(), RepositoryWatch{Repository: "not-a-repository", RegisteredAt: time.Now(), IssueCursor: 1})
	if err == nil {
		t.Fatal("invalid repository accepted")
	}
	_, err = database.RecordRepositoryWatchPoll(context.Background(), "owner/missing", 1, time.Now())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing watch error = %v", err)
	}
}

func newRepositoryWatchStore(t *testing.T) *Store {
	t.Helper()
	database, err := Open(context.Background(), filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}
