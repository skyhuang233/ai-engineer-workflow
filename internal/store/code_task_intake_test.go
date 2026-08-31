package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestCodeTaskReceiptIsIdempotentByRepositoryAndImmutableIssueID(t *testing.T) {
	database, err := Open(context.Background(), filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	now := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	input := CodeTaskReceipt{Repository: "owner/repository", GitHubIssueID: 44, TaskReference: CodeTaskReference("owner/repository", 44), SnapshotJSON: `{"id":44}`, AcceptedAt: now}
	receipt, inserted, err := database.AcceptCodeTaskIssue(context.Background(), input)
	if err != nil || !inserted || receipt.TaskReference != input.TaskReference {
		t.Fatalf("receipt=%+v inserted=%v err=%v", receipt, inserted, err)
	}
	receipt, inserted, err = database.AcceptCodeTaskIssue(context.Background(), CodeTaskReceipt{Repository: input.Repository, GitHubIssueID: input.GitHubIssueID, TaskReference: "different", SnapshotJSON: `{"changed":true}`, AcceptedAt: now.Add(time.Hour)})
	if err != nil || inserted || receipt.TaskReference != input.TaskReference || receipt.SnapshotJSON != input.SnapshotJSON {
		t.Fatalf("idempotent receipt=%+v inserted=%v err=%v", receipt, inserted, err)
	}
}
