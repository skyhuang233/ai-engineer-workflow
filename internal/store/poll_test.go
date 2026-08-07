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

func TestGitHubPollFailureAdoptsVerifiedBootstrapProvenanceAfterGenericFailure(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 8, 7, 6, 0, 0, 0, time.UTC)
	repository := "owner/repo"
	if err := db.AcquireGitHubPollLease(ctx, repository, "lease", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AdvanceGitHubPollFailureLeased(ctx, repository, now, GitHubPollFailureRetryable, "", "lease", now); err != nil {
		t.Fatal(err)
	}
	cursor, err := db.AdvanceGitHubPollFailureLeased(ctx, repository, now.Add(time.Second), GitHubPollFailurePreActivationInboxConflict, "version-current", "lease", now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if cursor.ConsecutiveFailures != 2 || cursor.FailureKind != GitHubPollFailurePreActivationInboxConflict || cursor.RecoveryState != GitHubPollRecoveryAvailable || cursor.RecoveryPlanVersionID != "version-current" {
		t.Fatalf("bootstrap cursor = %#v", cursor)
	}
}

func TestGitHubPollLeaseAtomicallyFencesRepositoryReadiness(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)
	repository := "owner/repo"
	if err := db.AcquireGitHubPollLease(ctx, repository, "first", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := db.AcquireGitHubPollLease(ctx, repository, "second", now, time.Minute); !errors.Is(err, ErrNotReady) {
		t.Fatalf("concurrent lease error = %v, want not ready", err)
	}
	if err := db.ReleaseGitHubPollLease(ctx, repository, "second", now); !errors.Is(err, ErrFencingConflict) {
		t.Fatalf("foreign release error = %v, want fencing conflict", err)
	}
	if err := db.ReleaseGitHubPollLease(ctx, repository, "first", now); err != nil {
		t.Fatal(err)
	}
	if err := db.DeferGitHubPoll(ctx, repository, now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	if err := db.AcquireGitHubPollLease(ctx, repository, "third", now, time.Minute); !errors.Is(err, ErrNotReady) {
		t.Fatalf("backoff lease error = %v, want not ready", err)
	}
	if err := db.AcquireGitHubPollLease(ctx, repository, "third", now.Add(time.Minute), time.Minute); err != nil {
		t.Fatal(err)
	}
}

func TestGitHubPollLeaseRejectsMutationAfterExpiry(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)
	repository := "owner/repo"
	if err := db.AcquireGitHubPollLease(ctx, repository, "lease", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	_, err = db.AdvanceGitHubPollFailureLeased(ctx, repository, now.Add(30*time.Second), GitHubPollFailureRetryable, "", "lease", now.Add(2*time.Minute))
	if !errors.Is(err, ErrFencingConflict) {
		t.Fatalf("expired lease mutation = %v, want fencing conflict", err)
	}
	cursor, err := db.GitHubPollCursor(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	if cursor.ConsecutiveFailures != 0 {
		t.Fatalf("expired lease persisted failures = %d", cursor.ConsecutiveFailures)
	}
}

func TestUnleasedGitHubPollMutationCannotBypassActiveLease(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC()
	repository := "owner/repo"
	if err := db.AcquireGitHubPollLease(ctx, repository, "poll-owner", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordGitHubPollFailure(ctx, repository, now); !errors.Is(err, ErrFencingConflict) {
		t.Fatalf("unleased mutation error = %v, want fencing conflict", err)
	}
	cursor, err := db.GitHubPollCursor(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	if cursor.ConsecutiveFailures != 0 {
		t.Fatalf("unleased mutation persisted %d failures", cursor.ConsecutiveFailures)
	}
}

func TestPollTerminalTransitionIsAtomic(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	snapshot := plan.Snapshot{
		Repository: "owner/repo",
		Root:       plan.Issue{ID: 1, Number: 1, Labels: []string{plan.PlanLabel}},
		Children:   []plan.Issue{{ID: 2, Number: 2, Labels: []string{plan.TicketLabel}, State: "open"}},
	}
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
	if _, err := db.db.ExecContext(ctx, `CREATE TRIGGER fail_poll_attention BEFORE UPDATE OF state ON ticket_runtime
WHEN NEW.state = 'needs_attention' BEGIN SELECT RAISE(ABORT, 'injected attention failure'); END`); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)
	if err := db.MarkGitHubPollFailureUnrecoverableAndRepositoryNeedsAttention(ctx, snapshot.Repository, now); err == nil {
		t.Fatal("terminal transition succeeded despite injected failure")
	}
	cursor, err := db.GitHubPollCursor(ctx, snapshot.Repository)
	if err != nil {
		t.Fatal(err)
	}
	if cursor.NeedsAttention() || cursor.ConsecutiveFailures != 0 || cursor.FailureKind != "" {
		t.Fatalf("terminal state survived rollback: %#v", cursor)
	}
	questions, err := db.OpenWorkflowQuestions(ctx, snapshot.Repository, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(questions) != 0 {
		t.Fatalf("recovery questions survived rollback: %#v", questions)
	}
}

func TestTerminalPollOwnerSurvivesAdmissionAndCredentialFailures(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := "owner/repository"
	now := time.Date(2026, 8, 7, 4, 0, 0, 0, time.UTC)
	versionID := activateWorkflowInboxPlanAt(t, ctx, db, repository, 1, 1, 2, 2)
	if err := db.MarkTicketDelivered(ctx, versionID, 2); err != nil {
		t.Fatal(err)
	}
	if err := db.AcquireGitHubPollLease(ctx, repository, "poll-lease", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkGitHubPollFailureUnrecoverableAndRepositoryNeedsAttentionForPlanLeased(ctx, repository, versionID, now, "poll-lease", now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AdvanceGitHubPollFailureLeased(ctx, repository, now.Add(time.Second), GitHubPollFailureRetryable, "", "poll-lease", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkGitHubPollFailureUnrecoverableLeased(ctx, repository, now.Add(2*time.Second), "poll-lease", now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := db.PauseGatewayWritesForGitHubPollCredential(ctx, repository, "poll-lease", "credential unavailable", now.Add(3*time.Second), now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	cursor, err := db.GitHubPollCursor(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	if cursor.FailureKind != GitHubPollFailureUnrecoverable || cursor.RecoveryState != GitHubPollRecoveryConsumed || cursor.RecoveryPlanVersionID != versionID {
		t.Fatalf("terminal cursor = %#v", cursor)
	}
	questions, err := db.WorkflowInboxQuestions(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, question := range questions {
		found = found || question.VersionID == versionID && question.Kind == "poll_failure"
	}
	if !found {
		t.Fatalf("terminal recovery questions = %#v", questions)
	}
}

func TestSchedulerRootUsesConfiguredRootBeforeFirstActivation(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	root, err := db.SchedulerRoot(ctx, "owner/repo", 10, time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC))
	if err != nil || root != 10 {
		t.Fatalf("fresh scheduler root = %d, %v", root, err)
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
	outbox, err := db.AnswerWorkflowQuestionAndQueueInboxProjection(ctx, snapshot.Repository, questions[0].ID, `{"action":"cancel-plan"}`, now)
	if err != nil {
		t.Fatal(err)
	}
	if outbox.IdempotencyKey == "" || len(outbox.Request.WorkflowQuestions) != 0 || outbox.Request.InboxProjectionGeneration == 0 {
		t.Fatalf("cancelled plan Inbox projection = %#v", outbox)
	}
	if _, err := db.ClaimReady(ctx, ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent", MaxParallelRuns: 1, LeaseTTL: time.Hour, Now: now.Add(time.Second)}); !errors.Is(err, ErrNotReady) {
		t.Fatalf("cancelled plan claim = %v, want not ready", err)
	}
}

func TestAnswerWorkflowQuestionRejectsSupersededPlanVersion(t *testing.T) {
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
	now := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureWorkflowQuestionTx(ctx, tx, snapshot.Repository, version.ID, 1, "needs_attention", "stale question", now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	questions, err := db.OpenWorkflowQuestions(ctx, snapshot.Repository, 0)
	if err != nil || len(questions) != 1 {
		t.Fatalf("questions = %#v, %v", questions, err)
	}
	tx, err = db.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.cancelPlanTx(ctx, tx, version.ID, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	replacementSnapshot := plan.Snapshot{
		Repository: snapshot.Repository,
		Root:       plan.Issue{ID: 200, Number: 20, Labels: []string{plan.PlanLabel}},
		Children:   []plan.Issue{{ID: 201, Number: 21, Labels: []string{plan.TicketLabel}, State: "open"}},
	}
	replacementFingerprint, err := replacementSnapshot.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := db.BeginActivation(ctx, replacementSnapshot, replacementFingerprint, "revision-2")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkActive(ctx, replacement.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.AnswerWorkflowQuestion(ctx, snapshot.Repository, questions[0].ID, "retry", now.Add(time.Second)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("superseded question answer = %v, want not found", err)
	}
	question, err := db.WorkflowQuestion(ctx, snapshot.Repository, questions[0].ID)
	if err != nil || question.State != "open" || question.Answer != "" {
		t.Fatalf("superseded question changed = %#v, %v", question, err)
	}
}

func TestReplacementRetiresClosedTicketAndPersistsApproval(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "workflow.db")
	db, err := Open(ctx, dbPath)
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
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
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

func TestWorkflowQuestionAnswerCannotRaceRepositoryPollLease(t *testing.T) {
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
	if err := db.MarkRepositoryNeedsAttention(ctx, snapshot.Repository, now); err != nil {
		t.Fatal(err)
	}
	questions, err := db.OpenWorkflowQuestions(ctx, snapshot.Repository, 0)
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
	if err := db.AcquireGitHubPollLease(ctx, snapshot.Repository, "active-poller", now.Add(time.Second), time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := db.AnswerWorkflowQuestion(ctx, snapshot.Repository, pollFailure.ID, "retry", now.Add(2*time.Second)); !errors.Is(err, ErrFencingConflict) {
		t.Fatalf("concurrent answer = %v, want fencing conflict", err)
	}
	question, err := db.WorkflowQuestion(ctx, snapshot.Repository, pollFailure.ID)
	if err != nil || question.State != "open" {
		t.Fatalf("question after fenced answer = %#v, %v", question, err)
	}
	if err := db.ReleaseGitHubPollLease(ctx, snapshot.Repository, "active-poller", now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := db.AnswerWorkflowQuestion(ctx, snapshot.Repository, pollFailure.ID, "retry", now.Add(3*time.Second)); err != nil {
		t.Fatalf("answer after lease release = %v", err)
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
