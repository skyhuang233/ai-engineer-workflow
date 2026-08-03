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

func TestClosedUnmergedQuestionRequiresTypedPlanDecision(t *testing.T) {
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
	if _, err := tx.ExecContext(ctx, `INSERT INTO plan_freezes(version_id, issue_id, reason, frozen_at) VALUES (?, ?, ?, ?)`, version.ID, int64(1), "closed pull request", formatTimestamp(now)); err != nil {
		t.Fatal(err)
	}
	if err := ensureWorkflowQuestionTx(ctx, tx, snapshot.Repository, version.ID, 0, "closed_unmerged_impact", "choose", now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	questions, err := db.OpenWorkflowQuestions(ctx, snapshot.Repository, 0)
	if err != nil || len(questions) != 1 {
		t.Fatalf("questions = %#v, %v", questions, err)
	}
	if err := db.AnswerWorkflowQuestion(ctx, snapshot.Repository, questions[0].ID, "retry", now); !errors.Is(err, ErrInvalidClaim) {
		t.Fatalf("free-text answer = %v, want invalid claim", err)
	}
	question, err := db.WorkflowQuestion(ctx, snapshot.Repository, questions[0].ID)
	if err != nil || question.State != "open" {
		t.Fatalf("invalid answer changed question = %#v, %v", question, err)
	}
	if err := db.AnswerWorkflowQuestion(ctx, snapshot.Repository, questions[0].ID, `{"action":"cancel-plan"}`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimReady(ctx, ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent", MaxParallelRuns: 1, LeaseTTL: time.Hour, Now: now.Add(time.Second)}); !errors.Is(err, ErrNotReady) {
		t.Fatalf("cancelled plan claim = %v, want not ready", err)
	}
}

func TestReplacementRetiresClosedTicketAndPersistsApproval(t *testing.T) {
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
	replacementSnapshot := testSnapshot()
	replacementSnapshot.Root.ID = 200
	replacementSnapshot.Root.Number = 20
	replacementSnapshot.Children[0].ID = 101
	replacementSnapshot.Children[0].Number = 21
	replacementSnapshot.Children[1].ID = 102
	replacementSnapshot.Children[1].Number = 22
	replacementSnapshot.BlockedBy = map[int64][]plan.Issue{102: {{ID: 101, Number: 21, Labels: []string{plan.TicketLabel}, State: "open"}}}
	replacementFingerprint, err := replacementSnapshot.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	replacementVersion, err := db.BeginActivation(ctx, replacementSnapshot, replacementFingerprint, "revision-2")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	if _, err := db.FreezePlanForClosedPullRequest(ctx, version.ID, 1, now); err != nil {
		t.Fatal(err)
	}
	questions, err := db.OpenWorkflowQuestions(ctx, snapshot.Repository, snapshot.Root.Number)
	if err != nil || len(questions) == 0 {
		t.Fatalf("questions = %#v, %v", questions, err)
	}
	var question WorkflowQuestion
	for _, candidate := range questions {
		if candidate.Kind == "closed_unmerged_impact" {
			question = candidate
			break
		}
	}
	if question.ID == "" || question.IssueID != 1 {
		t.Fatalf("closed question = %#v", question)
	}
	answer := `{"action":"replace","replacement":{"plan_root_issue_id":200}}`
	if err := db.AnswerWorkflowQuestion(ctx, snapshot.Repository, question.ID, answer, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := db.AnswerWorkflowQuestion(ctx, snapshot.Repository, question.ID, answer, now.Add(2*time.Second)); err != nil {
		t.Fatalf("idempotent replacement = %v", err)
	}
	var replacement, state, replacementVersionID string
	var retiredID, replacementIssueID int64
	if err := db.db.QueryRowContext(ctx, `SELECT replacement, state, retired_issue_id, replacement_version_id, replacement_issue_id FROM replacement_tickets WHERE question_id = ?`, question.ID).Scan(&replacement, &state, &retiredID, &replacementVersionID, &replacementIssueID); err != nil {
		t.Fatal(err)
	}
	if replacement != `{"plan_root_issue_id":200}` || state != "approved" || retiredID != 1 || replacementVersionID != replacementVersion.ID || replacementIssueID != 0 {
		t.Fatalf("replacement = %q, %q, %d, %q, %d", replacement, state, retiredID, replacementVersionID, replacementIssueID)
	}
	activatedReplacement, err := db.CurrentVersion(ctx, snapshot.Repository, replacementSnapshot.Root.ID)
	if err != nil || activatedReplacement.State != StateProjecting {
		t.Fatalf("replacement activation = %#v, %v", activatedReplacement, err)
	}
	selectedRoot, err := db.SchedulerRoot(ctx, snapshot.Repository, snapshot.Root.Number, now.Add(2*time.Second))
	if err != nil || selectedRoot != replacementSnapshot.Root.Number {
		t.Fatalf("projecting replacement root = %d, %v", selectedRoot, err)
	}
	frontier, err := db.ReadyFrontier(ctx, version.ID, 2, now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(frontier) != 0 {
		t.Fatalf("retired ticket was dispatched: %#v", frontier)
	}
	var runtime string
	if err := db.db.QueryRowContext(ctx, `SELECT state FROM ticket_runtime WHERE version_id = ? AND issue_id = ?`, version.ID, int64(1)).Scan(&runtime); err != nil {
		t.Fatal(err)
	}
	if runtime == plan.StateCancelled {
		t.Fatalf("source was cancelled before replacement projection confirmation")
	}
	if err := db.MarkActive(ctx, replacementVersion.ID); err != nil {
		t.Fatal(err)
	}
	selectedRoot, err = db.SchedulerRoot(ctx, snapshot.Repository, snapshot.Root.Number, now.Add(4*time.Second))
	if err != nil || selectedRoot != replacementSnapshot.Root.Number {
		t.Fatalf("active replacement root = %d, %v", selectedRoot, err)
	}
	if state, err := db.CurrentVersion(ctx, snapshot.Repository, snapshot.Root.ID); err != nil || state.State != "cancelled" {
		t.Fatalf("source after handoff = %#v, %v", state, err)
	}
	replacementFrontier, err := db.ReadyFrontier(ctx, replacementVersion.ID, 2, now.Add(5*time.Second))
	if err != nil || len(replacementFrontier) != 1 || replacementFrontier[0].IssueID != 101 {
		t.Fatalf("replacement frontier = %#v, %v", replacementFrontier, err)
	}
	if _, err := db.ClaimReady(ctx, ClaimRequest{VersionID: replacementVersion.ID, TicketID: 101, Owner: "replacement-agent", MaxParallelRuns: 2, LeaseTTL: time.Hour, Now: now.Add(5 * time.Second)}); err != nil {
		t.Fatalf("replacement handoff was not schedulable: %v", err)
	}
}

func TestReplacementRequiresAnAuthoritativeReference(t *testing.T) {
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
	if _, err := db.FreezePlanForClosedPullRequest(ctx, version.ID, 1, now); err != nil {
		t.Fatal(err)
	}
	questions, err := db.OpenWorkflowQuestions(ctx, snapshot.Repository, snapshot.Root.Number)
	if err != nil {
		t.Fatalf("questions = %#v, %v", questions, err)
	}
	var closedQuestion WorkflowQuestion
	for _, candidate := range questions {
		if candidate.Kind == "closed_unmerged_impact" {
			closedQuestion = candidate
			break
		}
	}
	if closedQuestion.ID == "" {
		t.Fatalf("closed question missing from %#v", questions)
	}
	if err := db.AnswerWorkflowQuestion(ctx, snapshot.Repository, closedQuestion.ID, `{"action":"replace","replacement":"ticket #99"}`, now); !errors.Is(err, ErrInvalidClaim) {
		t.Fatalf("free-text replacement = %v, want invalid claim", err)
	}
	question, err := db.WorkflowQuestion(ctx, snapshot.Repository, closedQuestion.ID)
	if err != nil || question.State != "open" {
		t.Fatalf("invalid replacement changed question = %#v, %v", question, err)
	}
}

func TestPollFailureSkipsTerminalAndFrozenPlans(t *testing.T) {
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
	if _, err := db.db.ExecContext(ctx, `INSERT INTO plan_terminal_states(version_id, state, recorded_at) VALUES (?, ?, ?)`, version.ID, "cancelled", formatTimestamp(now)); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkRepositoryNeedsAttention(ctx, snapshot.Repository, now); err != nil {
		t.Fatal(err)
	}
	questions, err := db.OpenWorkflowQuestions(ctx, snapshot.Repository, snapshot.Root.Number)
	if err != nil {
		t.Fatal(err)
	}
	if len(questions) != 0 {
		t.Fatalf("terminal poll questions = %#v", questions)
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
	delivery, err := db.AcceptCandidateForDelivery(ctx, CandidateRevision{RunID: claim.RunID, LeaseToken: claim.LeaseToken, CodexSessionID: "codex", CommitSHA: "accepted", StructuredOutput: []byte(`{"summary":"candidate","checks":[{"command":"go test","outcome":"passed"}]}`), Now: now, Publication: CandidatePublication{Repository: snapshot.Repository, Branch: "ticket-1", ExpectRemoteAbsent: true, Title: "ticket"}}, time.Hour)
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
	if claims, err := db.ClaimPendingDeliveryClaims(ctx, snapshot.Repository, 1, time.Hour, now.Add(2*time.Hour+time.Second)); err != nil || len(claims) != 1 {
		t.Fatalf("claimed recovered delivery = %#v, %v", claims, err)
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

func TestPollFailureAnswerRetriesAcceptedCandidateWithDeliveryLease(t *testing.T) {
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
	if _, err := db.AcceptCandidateForDelivery(ctx, CandidateRevision{RunID: claim.RunID, LeaseToken: claim.LeaseToken, CodexSessionID: "codex", CommitSHA: "accepted", StructuredOutput: []byte(`{"summary":"candidate","checks":[{"command":"go test","outcome":"passed"}]}`), Now: now, Publication: CandidatePublication{Repository: snapshot.Repository, Branch: "ticket-1", ExpectRemoteAbsent: true, Title: "ticket"}}, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkRepositoryNeedsAttention(ctx, snapshot.Repository, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	questions, err := db.OpenWorkflowQuestions(ctx, snapshot.Repository, snapshot.Root.Number)
	if err != nil {
		t.Fatal(err)
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
	if err := db.AnswerWorkflowQuestion(ctx, snapshot.Repository, pollFailure.ID, "retry", now.Add(2*time.Hour+time.Second)); err != nil {
		t.Fatal(err)
	}
	if claims, err := db.ClaimPendingDeliveryClaims(ctx, snapshot.Repository, 1, time.Hour, now.Add(2*time.Hour+time.Second)); err != nil || len(claims) != 1 {
		t.Fatalf("claimed recovered delivery = %#v, %v", claims, err)
	}
	pending, err := db.PendingDeliveryClaims(ctx, snapshot.Repository, now.Add(2*time.Hour+time.Second))
	if err != nil || len(pending) != 1 || pending[0].TicketID != claim.TicketID {
		t.Fatalf("recovered delivery claims = %#v, err = %v", pending, err)
	}
	if _, err := db.ClaimReady(ctx, ClaimRequest{VersionID: version.ID, TicketID: claim.TicketID, Owner: "replacement", MaxParallelRuns: 1, LeaseTTL: time.Hour, Now: now.Add(2*time.Hour + 2*time.Second)}); !errors.Is(err, ErrFencingConflict) {
		t.Fatalf("Agent recovery after poll failure = %v, want ErrFencingConflict", err)
	}
}

func TestRecoveredDeliveryWaitsForGlobalCapacity(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	snapshot := testSnapshot()
	snapshot.BlockedBy = map[int64][]plan.Issue{}
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
	first, err := db.ClaimReady(ctx, ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent-1", MaxParallelRuns: 1, LeaseTTL: time.Hour, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	firstDelivery, err := db.AcceptCandidateForDelivery(ctx, CandidateRevision{RunID: first.RunID, LeaseToken: first.LeaseToken, CodexSessionID: "codex-1", CommitSHA: "accepted-1", StructuredOutput: []byte(`{"summary":"candidate","checks":[{"command":"go test","outcome":"passed"}]}`), Now: now, Publication: CandidatePublication{Repository: snapshot.Repository, Branch: "ticket-1", ExpectRemoteAbsent: true, Title: "ticket-1"}}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FailDeliveryController(ctx, firstDelivery, "delivery failed", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	questions, err := db.OpenWorkflowQuestions(ctx, snapshot.Repository, snapshot.Root.Number)
	if err != nil || len(questions) != 1 {
		t.Fatalf("questions = %#v, %v", questions, err)
	}
	if err := db.AnswerWorkflowQuestion(ctx, snapshot.Repository, questions[0].ID, "retry", now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	second, err := db.ClaimReady(ctx, ClaimRequest{VersionID: version.ID, TicketID: 2, Owner: "agent-2", MaxParallelRuns: 1, LeaseTTL: time.Hour, Now: now.Add(2 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcceptCandidateForDelivery(ctx, CandidateRevision{RunID: second.RunID, LeaseToken: second.LeaseToken, CodexSessionID: "codex-2", CommitSHA: "accepted-2", StructuredOutput: []byte(`{"summary":"candidate","checks":[{"command":"go test","outcome":"passed"}]}`), Now: now.Add(2 * time.Second), Publication: CandidatePublication{Repository: snapshot.Repository, Branch: "ticket-2", ExpectRemoteAbsent: true, Title: "ticket-2"}}, time.Hour); err != nil {
		t.Fatal(err)
	}
	claims, err := db.ClaimPendingDeliveryClaims(ctx, snapshot.Repository, 1, time.Hour, now.Add(3*time.Second))
	if err != nil || len(claims) != 0 {
		t.Fatalf("recovered claims while delivery is live = %#v, %v", claims, err)
	}
	var pending int
	if err := db.db.QueryRowContext(ctx, `SELECT delivery_retry_pending FROM ticket_sessions WHERE session_id = ?`, first.SessionID).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Fatalf("delivery retry pending = %d, want 1", pending)
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
