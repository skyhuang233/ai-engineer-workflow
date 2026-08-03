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
	if err := db.RecordGitHubPollSuccess(ctx, "owner/repo", now.Add(time.Minute), true); err != nil {
		t.Fatal(err)
	}
	cursor, err = db.GitHubPollCursor(ctx, "owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	if cursor.ConsecutiveFailures != 0 || !cursor.NextAttemptAt.Equal(now.Add(time.Minute)) || !cursor.LastFullReconcileAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("success cursor = %#v", cursor)
	}
	if err := db.RecordGitHubPollSuccess(ctx, "owner/repo", now.Add(2*time.Minute), false); err != nil {
		t.Fatal(err)
	}
	cursor, err = db.GitHubPollCursor(ctx, "owner/repo")
	if err != nil || !cursor.LastFullReconcileAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("incremental cursor = %#v, %v", cursor, err)
	}
}

func TestNeedsAttentionAnswerRestoresTicketAndOpensNextGeneration(t *testing.T) {
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
	now := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	claim, err := db.ClaimReady(ctx, ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent", MaxParallelRuns: 1, LeaseTTL: time.Hour, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO plan_freezes(version_id, issue_id, reason, frozen_at) VALUES (?, ?, ?, ?)`, version.ID, claim.TicketID, "blocked", formatTimestamp(now)); err != nil {
		t.Fatal(err)
	}
	if err := markTicketNeedsAttentionTx(ctx, tx, version.ID, claim.TicketID, "retry required", now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	questions, err := db.OpenWorkflowQuestions(ctx, snapshot.Repository, snapshot.Root.Number)
	if err != nil || len(questions) != 1 {
		t.Fatalf("questions = %#v, %v", questions, err)
	}
	if err := db.AnswerWorkflowQuestion(ctx, snapshot.Repository, questions[0].ID, "retry", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimReady(ctx, ClaimRequest{VersionID: version.ID, TicketID: claim.TicketID, Owner: "agent", MaxParallelRuns: 1, LeaseTTL: time.Hour, Now: now.Add(2 * time.Second)}); err != nil {
		t.Fatalf("reclaimed answered ticket: %v", err)
	}
	tx, err = db.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := markTicketNeedsAttentionTx(ctx, tx, version.ID, claim.TicketID, "retry again", now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	questions, err = db.OpenWorkflowQuestions(ctx, snapshot.Repository, snapshot.Root.Number)
	if err != nil || len(questions) != 1 || questions[0].ID != "needs-attention-"+version.ID+"-1-g2" {
		t.Fatalf("reopened question = %#v, %v", questions, err)
	}
}

func TestPollFailureAnswerRestoresPausedTickets(t *testing.T) {
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
	now := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	if _, err := db.ClaimReady(ctx, ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent", MaxParallelRuns: 1, MaxAttempts: 1, LeaseTTL: time.Hour, Now: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkRepositoryNeedsAttention(ctx, snapshot.Repository, now); err != nil {
		t.Fatal(err)
	}
	questions, err := db.OpenWorkflowQuestions(ctx, snapshot.Repository, 0)
	if err != nil || len(questions) != 3 {
		t.Fatalf("questions = %#v, %v", questions, err)
	}
	var pollFailure WorkflowQuestion
	for _, question := range questions {
		if question.Kind == "poll_failure" {
			pollFailure = question
		}
	}
	if pollFailure.ID == "" {
		t.Fatal("poll failure question was not created")
	}
	if err := db.AnswerWorkflowQuestion(ctx, snapshot.Repository, pollFailure.ID, "retry", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var recoveryEpoch int
	if err := db.db.QueryRowContext(ctx, `SELECT recovery_epoch FROM ticket_sessions WHERE version_id = ? AND issue_id = ?`, version.ID, int64(1)).Scan(&recoveryEpoch); err != nil {
		t.Fatal(err)
	}
	if recoveryEpoch != 1 {
		t.Fatalf("recovery epoch = %d, want 1", recoveryEpoch)
	}
	if _, err := db.ClaimReady(ctx, ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent", MaxParallelRuns: 1, MaxAttempts: 1, LeaseTTL: time.Hour, Now: now.Add(2 * time.Second)}); err != nil {
		t.Fatalf("reclaimed ticket after poll failure: %v", err)
	}
}

func TestTicketAnswerDoesNotClearAnotherTicketsPlanFreeze(t *testing.T) {
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
	now := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO plan_freezes(version_id, issue_id, reason, frozen_at) VALUES (?, ?, ?, ?)`, version.ID, 1, "unmerged", formatTimestamp(now)); err != nil {
		t.Fatal(err)
	}
	if err := markTicketNeedsAttentionTx(ctx, tx, version.ID, 2, "retry required", now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	questions, err := db.OpenWorkflowQuestions(ctx, snapshot.Repository, 0)
	if err != nil || len(questions) != 1 {
		t.Fatalf("questions = %#v, %v", questions, err)
	}
	if err := db.AnswerWorkflowQuestion(ctx, snapshot.Repository, questions[0].ID, "retry", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var frozenIssueID int64
	if err := db.db.QueryRowContext(ctx, `SELECT issue_id FROM plan_freezes WHERE version_id = ?`, version.ID).Scan(&frozenIssueID); err != nil {
		t.Fatal(err)
	}
	if frozenIssueID != 1 {
		t.Fatalf("freeze issue = %d, want 1", frozenIssueID)
	}
}
