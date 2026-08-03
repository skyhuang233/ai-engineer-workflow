package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestGitHubPollCursorPersistsBackoffAndRecovery(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	if err := db.RecordGitHubPollFailure(ctx, "owner/repo", now); err != nil {
		t.Fatal(err)
	}
	cursor, err := db.GitHubPollCursor(ctx, "owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	if cursor.ConsecutiveFailures != 1 || !cursor.NextAttemptAt.Equal(now.Add(time.Second)) {
		t.Fatalf("failure cursor = %#v", cursor)
	}
	if err := db.RecordGitHubPollSuccess(ctx, "owner/repo", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	cursor, err = db.GitHubPollCursor(ctx, "owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	if cursor.ConsecutiveFailures != 0 || !cursor.NextAttemptAt.Equal(now.Add(time.Minute)) || !cursor.LastFullReconcileAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("success cursor = %#v", cursor)
	}
}
