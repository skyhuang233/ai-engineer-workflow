package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/skyhuang233/workflow/internal/plan"
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

func TestNeedsAttentionAnswerRetriesAcceptedCandidateWithDeliveryLease(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "workflow.db")
	db, err := Open(ctx, databasePath)
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
	delivery, err := db.AcceptCandidateForDelivery(ctx, CandidateRevision{RunID: claim.RunID, LeaseToken: claim.LeaseToken, CodexSessionID: "codex", CommitSHA: "accepted", StructuredOutput: []byte(`{"summary":"candidate"}`), Now: now, Publication: CandidatePublication{Repository: snapshot.Repository, Branch: "ticket-1", ExpectRemoteAbsent: true, Title: "ticket"}}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FailDeliveryController(ctx, delivery, "Delivery Controller lease expired before completion", now.Add(2*time.Hour)); !errors.Is(err, ErrNeedsAttention) {
		t.Fatalf("expire delivery = %v, want ErrNeedsAttention", err)
	}
	questions, err := db.OpenWorkflowQuestions(ctx, snapshot.Repository, snapshot.Root.Number)
	if err != nil || len(questions) != 1 {
		t.Fatalf("questions = %#v, %v", questions, err)
	}
	if err := db.AnswerWorkflowQuestion(ctx, snapshot.Repository, questions[0].ID, "retry", now.Add(2*time.Hour+time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	pending, err := db.PendingDeliveryClaims(ctx, snapshot.Repository, now.Add(2*time.Hour+time.Second))
	if err != nil || len(pending) != 1 {
		t.Fatalf("persisted delivery claims = %#v, %v", pending, err)
	}
	var runKind, runState, leaseState, acceptedCommit string
	if err := db.db.QueryRowContext(ctx, `SELECT r.run_kind, r.state, l.state, s.accepted_commit
FROM ticket_sessions s
JOIN worker_runs r ON r.run_id = s.current_run_id
JOIN run_leases l ON l.run_id = r.run_id AND l.generation = r.lease_generation
WHERE s.version_id = ? AND s.issue_id = ?`, version.ID, claim.TicketID).Scan(&runKind, &runState, &leaseState, &acceptedCommit); err != nil {
		t.Fatal(err)
	}
	if runKind != RunDelivery || runState != RunRunning || leaseState != LeaseActive || acceptedCommit != "accepted" {
		t.Fatalf("retried delivery = kind %q, state %q, lease %q, commit %q", runKind, runState, leaseState, acceptedCommit)
	}
	if _, err := db.ClaimReady(ctx, ClaimRequest{VersionID: version.ID, TicketID: claim.TicketID, Owner: "replacement", MaxParallelRuns: 1, LeaseTTL: time.Hour, Now: now.Add(2*time.Hour + 2*time.Second)}); !errors.Is(err, ErrFencingConflict) {
		t.Fatalf("Agent recovery after delivery retry = %v, want ErrFencingConflict", err)
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

func TestRepositoryEscalationRequeuesClaimedReviewFeedback(t *testing.T) {
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
	if _, err := db.db.ExecContext(ctx, `UPDATE worker_runs SET state = 'succeeded' WHERE run_id = ?`, claim.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `UPDATE run_leases SET state = 'revoked' WHERE run_id = ?`, claim.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `UPDATE ticket_runtime SET state = ? WHERE version_id = ? AND issue_id = ?`, plan.StateWaitingReview, version.ID, claim.TicketID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecordReviewFeedback(ctx, version.ID, claim.TicketID, []ReviewFeedback{{Source: "review", EventID: "1", Author: "human", Body: "Please revise."}}, now); err != nil {
		t.Fatal(err)
	}
	revision, _, err := db.ClaimQueuedReviewRevision(ctx, version.ID, claim.TicketID, time.Hour, now.Add(time.Second), 1, DefaultMaxWorkerAttempts)
	if err != nil {
		t.Fatal(err)
	}
	var claimedRunID string
	if err := db.db.QueryRowContext(ctx, `SELECT claimed_run_id FROM review_feedback_events WHERE version_id = ? AND issue_id = ? AND source = ? AND event_id = ?`, version.ID, claim.TicketID, "review", "1").Scan(&claimedRunID); err != nil {
		t.Fatal(err)
	}
	if claimedRunID != revision.RunID {
		t.Fatalf("claimed run = %q, want %q", claimedRunID, revision.RunID)
	}
	if err := db.MarkRepositoryNeedsAttention(ctx, snapshot.Repository, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRowContext(ctx, `SELECT claimed_run_id FROM review_feedback_events WHERE version_id = ? AND issue_id = ? AND source = ? AND event_id = ?`, version.ID, claim.TicketID, "review", "1").Scan(&claimedRunID); err != nil {
		t.Fatal(err)
	}
	if claimedRunID != "" {
		t.Fatalf("claimed run after escalation = %q, want empty", claimedRunID)
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
