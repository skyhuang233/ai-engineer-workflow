package github

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/skyhuang233/workflow/internal/store"
)

func TestRecordFailurePersistsAfterParentCancellation(t *testing.T) {
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	now := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	_, err = (Poller{Store: db}).recordFailure(ctx, "owner/repo", now, context.DeadlineExceeded)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("record failure error = %v", err)
	}
	cursor, err := db.GitHubPollCursor(context.Background(), "owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	if cursor.ConsecutiveFailures != 1 {
		t.Fatalf("consecutive failures = %d, want 1", cursor.ConsecutiveFailures)
	}
}
