package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestRecordPullRequestChecksPersistsStateChanges(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	snapshot := testSnapshot()
	fingerprint, err := snapshot.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	version, err := db.BeginActivation(ctx, snapshot, fingerprint, "revision-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkActive(ctx, version.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	check := PullRequestCheck{CheckRunID: 42, Name: "workflow-contract", Status: "in_progress", HeadSHA: "candidate"}
	updated, err := db.RecordPullRequestChecks(ctx, version.ID, 1, []PullRequestCheck{check}, now)
	if err != nil || updated != 1 {
		t.Fatalf("initial record = %d, %v", updated, err)
	}
	updated, err = db.RecordPullRequestChecks(ctx, version.ID, 1, []PullRequestCheck{check}, now.Add(time.Second))
	if err != nil || updated != 0 {
		t.Fatalf("unchanged record = %d, %v", updated, err)
	}
	check.Status, check.Conclusion = "completed", "success"
	updated, err = db.RecordPullRequestChecks(ctx, version.ID, 1, []PullRequestCheck{check}, now.Add(2*time.Second))
	if err != nil || updated != 1 {
		t.Fatalf("changed record = %d, %v", updated, err)
	}
}
