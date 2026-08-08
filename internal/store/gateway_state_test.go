package store

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/skyhuang233/workflow/internal/plan"
)

func TestGatewayCredentialPauseUsesOneDurableInboxItemAndResumes(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	first := time.Date(2026, 7, 31, 3, 0, 0, 0, time.UTC)
	activateWorkflowInboxPlan(t, ctx, db, "owner/repository")
	if err := db.PauseGatewayWrites(ctx, "credential rejected", first); err != nil {
		t.Fatal(err)
	}
	if err := db.PauseGatewayWrites(ctx, "credential still rejected", first.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	paused, reason, err := db.GatewayWritesPaused(ctx)
	if err != nil || !paused || reason != "credential still rejected" {
		t.Fatalf("pause = %t, %q, %v", paused, reason, err)
	}
	item, err := db.WorkflowInboxItem(ctx, GatewayCredentialInboxKey)
	if err != nil || item.State != "open" || !item.CreatedAt.Equal(first) {
		t.Fatalf("inbox item = %#v, %v", item, err)
	}
	questions, err := db.OpenWorkflowQuestions(ctx, "owner/repository", 0)
	if err != nil || len(questions) != 1 || questions[0].Kind != gatewayCredentialQuestionKind {
		t.Fatalf("credential recovery questions = %#v, %v", questions, err)
	}
	repositories, err := db.GatewayCredentialAttentionRepositories(ctx)
	if err != nil || len(repositories) != 1 || repositories[0] != "owner/repository" {
		t.Fatalf("credential recovery repositories = %#v, %v", repositories, err)
	}
	rotation, err := db.BeginGatewayCredentialRotation(ctx, "rotation-a", "credential rotation", first.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ResumeGatewayWrites(ctx, rotation, first.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	paused, _, err = db.GatewayWritesPaused(ctx)
	item, itemErr := db.WorkflowInboxItem(ctx, GatewayCredentialInboxKey)
	if err != nil || itemErr != nil || paused || item.State != "resolved" {
		t.Fatalf("resumed pause=%t item=%#v errors=%v/%v", paused, item, err, itemErr)
	}
	questions, err = db.OpenWorkflowQuestions(ctx, "owner/repository", 0)
	if err != nil || len(questions) != 0 {
		t.Fatalf("unresolved credential recovery questions = %#v, %v", questions, err)
	}
}

func TestGitHubPollCredentialPauseRollsBackCursorWithGatewayState(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := "owner/repository"
	now := time.Date(2026, 8, 7, 0, 0, 0, 0, time.UTC)
	if err := db.RecordGitHubPollFailure(ctx, repository, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := db.AcquireGitHubPollLease(ctx, repository, "poll-lease", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `CREATE TRIGGER fail_gateway_poll_pause BEFORE UPDATE OF writes_paused ON gateway_runtime
BEGIN SELECT RAISE(ABORT, 'injected Gateway pause failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := db.PauseGatewayWritesForGitHubPollCredential(ctx, repository, "poll-lease", "credential unavailable", now, now); err == nil {
		t.Fatal("credential pause succeeded despite injected Gateway failure")
	}
	cursor, err := db.GitHubPollCursor(ctx, repository)
	if err != nil || cursor.ConsecutiveFailures != 1 || cursor.RecoveryState != GitHubPollRecoveryConsumed {
		t.Fatalf("rolled-back poll cursor = %#v, %v", cursor, err)
	}
	paused, _, err := db.GatewayWritesPaused(ctx)
	if err != nil || paused {
		t.Fatalf("rolled-back Gateway pause = %t, %v", paused, err)
	}
}

func TestGatewayCredentialRotationRequiresLiveOwnerToResume(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	first, err := db.BeginGatewayCredentialRotation(ctx, "rotation-a", "credential rotation", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.BeginGatewayCredentialRotation(ctx, "rotation-b", "credential rotation", now.Add(time.Minute)); !errors.Is(err, ErrDeliveryInProgress) {
		t.Fatalf("concurrent rotation error = %v", err)
	}
	if err := db.ResumeGatewayWrites(ctx, GatewayCredentialRotation{Owner: "rotation-b", Generation: first.Generation}, now.Add(time.Minute)); !errors.Is(err, ErrFencingConflict) {
		t.Fatalf("foreign resume error = %v", err)
	}
	second, err := db.BeginGatewayCredentialRotation(ctx, "rotation-b", "credential rotation", now.Add(6*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if second.Generation != first.Generation+1 {
		t.Fatalf("generation = %d, want %d", second.Generation, first.Generation+1)
	}
	if err := db.ResumeGatewayWrites(ctx, first, now.Add(6*time.Minute)); !errors.Is(err, ErrFencingConflict) {
		t.Fatalf("stale resume error = %v", err)
	}
	if err := db.ResumeGatewayWrites(ctx, second, now.Add(6*time.Minute)); err != nil {
		t.Fatal(err)
	}
}

func TestGatewayPauseFencesNewDispatchAdmissionsAndWaitsForExistingOnes(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	activateWorkflowInboxPlan(t, ctx, db, "owner/repository")
	queued, err := db.EnqueueDelivery(ctx, DeliveryRequest{Operation: DeliveryProjectInbox, Repository: "owner/repository"}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	claim, err := db.ClaimDeliveryOutbox(ctx, queued.IdempotencyKey, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PauseGatewayWrites(ctx, "credential rotation", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimDeliveryOutbox(ctx, queued.IdempotencyKey, time.Now().UTC()); !errors.Is(err, ErrGatewayWritesPaused) {
		t.Fatalf("claim while paused error = %v, want ErrGatewayWritesPaused", err)
	}
	quiesced := make(chan error, 1)
	go func() { quiesced <- db.WaitForGatewayWritesQuiesced(ctx) }()
	select {
	case err := <-quiesced:
		t.Fatalf("wait returned before existing dispatch completed: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	if err := db.FinishDeliveryOutbox(ctx, queued.IdempotencyKey, claim.ClaimToken, OutboxSucceeded, "", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := <-quiesced; err != nil {
		t.Fatal(err)
	}
}

func TestPausedGatewayRecoversStaleControlPlaneClaimOnlyAfterDispatcherExpires(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	activateWorkflowInboxPlan(t, ctx, db, "owner/repository")
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	queued, err := db.EnqueueDelivery(ctx, DeliveryRequest{Operation: DeliveryProjectInbox, Repository: "owner/repository"}, now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	claim, err := db.ClaimDeliveryOutbox(ctx, queued.IdempotencyKey, now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PauseGatewayWrites(ctx, "credential rotation", now); err != nil {
		t.Fatal(err)
	}
	if err := db.RecoverExpiredGatewayDeliveryClaims(ctx, now); err != nil {
		t.Fatal(err)
	}
	outbox, err := db.DeliveryOutbox(ctx, queued.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if outbox.State != OutboxProcessing || outbox.ClaimToken != claim.ClaimToken {
		t.Fatalf("live dispatcher outbox = %#v", outbox)
	}
	if err := db.RecoverExpiredGatewayDeliveryClaims(ctx, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	outbox, err = db.DeliveryOutbox(ctx, queued.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if outbox.State != OutboxPending || !outbox.Uncertain || outbox.ClaimToken != "" {
		t.Fatalf("recovered stale outbox = %#v", outbox)
	}
}

func TestGatewayDispatcherRenewalRetainsControlPlaneClaim(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	activateWorkflowInboxPlan(t, ctx, db, "owner/repository")
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	queued, err := db.EnqueueDelivery(ctx, DeliveryRequest{Operation: DeliveryProjectInbox, Repository: "owner/repository"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimDeliveryOutbox(ctx, queued.IdempotencyKey, now); err != nil {
		t.Fatal(err)
	}
	if err := db.RenewGatewayDispatcher(ctx, "legacy-gateway-dispatcher", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := db.PauseGatewayWrites(ctx, "credential rotation", now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := db.RecoverExpiredGatewayDeliveryClaims(ctx, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	outbox, err := db.DeliveryOutbox(ctx, queued.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if outbox.State != OutboxProcessing {
		t.Fatalf("renewed control-plane outbox = %#v", outbox)
	}
}

func TestWorkflowInboxProjectionRequiresActiveDeliveryPlan(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.EnqueueDelivery(ctx, DeliveryRequest{Operation: DeliveryProjectInbox, Repository: "owner/unplanned"}, time.Now().UTC()); !errors.Is(err, ErrDeliveryRejected) {
		t.Fatalf("unplanned inbox projection error = %v, want delivery rejection", err)
	}
	projectingSnapshot := plan.Snapshot{
		Repository: "owner/projecting",
		Root:       plan.Issue{ID: 11, Number: 11, Labels: []string{plan.PlanLabel}},
		Children:   []plan.Issue{{ID: 12, Number: 12, Labels: []string{plan.TicketLabel}, State: "open"}},
	}
	fingerprint, err := projectingSnapshot.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.BeginActivation(ctx, projectingSnapshot, fingerprint, "projecting-inbox"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.EnqueueDelivery(ctx, DeliveryRequest{
		Operation: DeliveryProjectInbox, Repository: projectingSnapshot.Repository,
		WorkflowQuestions: []plan.WorkflowQuestion{{ID: "payload-must-not-authorize"}},
	}, time.Now().UTC()); !errors.Is(err, ErrNoActiveDeliveryPlan) {
		t.Fatalf("projecting-plan inbox projection error = %v, want no active delivery plan", err)
	}
	activateWorkflowInboxPlan(t, ctx, db, "owner/admitted")
	if _, err := db.EnqueueDelivery(ctx, DeliveryRequest{Operation: DeliveryProjectInbox, Repository: "owner/admitted"}, time.Now().UTC()); err != nil {
		t.Fatalf("active-plan inbox projection error = %v", err)
	}
	var completedVersionID string
	if err := db.db.QueryRowContext(ctx, "SELECT current_version_id FROM plans WHERE repository = ?", "owner/admitted").Scan(&completedVersionID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, "INSERT INTO completed_plan_versions(version_id, completed_at) VALUES (?, ?)", completedVersionID, formatTimestamp(time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	if _, err := db.EnqueueDelivery(ctx, DeliveryRequest{Operation: DeliveryProjectInbox, Repository: "owner/admitted"}, time.Now().UTC()); !errors.Is(err, ErrNoActiveDeliveryPlan) {
		t.Fatalf("completed-plan inbox projection error = %v, want no active delivery plan", err)
	}
}

func TestAdmittedWorkflowInboxReplayRequiresCurrentActivePlan(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := "owner/repository"
	activateWorkflowInboxPlan(t, ctx, db, repository)
	now := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)
	queued, err := db.EnqueueDelivery(ctx, DeliveryRequest{
		Operation: DeliveryProjectInbox, Repository: repository,
		WorkflowQuestions: []plan.WorkflowQuestion{{ID: "admitted"}},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := db.ClaimDeliveryOutbox(ctx, queued.IdempotencyKey, now)
	if err != nil {
		t.Fatal(err)
	}
	var versionID string
	if err := db.db.QueryRowContext(ctx, "SELECT current_version_id FROM plans WHERE repository = ?", repository).Scan(&versionID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, "UPDATE plan_versions SET state = ? WHERE version_id = ?", StateProjecting, versionID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, "UPDATE plans SET state = ? WHERE repository = ?", StateProjecting, repository); err != nil {
		t.Fatal(err)
	}
	applyCalls := 0
	apply := func(context.Context, DeliveryRequest) (DeliveryResult, error) {
		applyCalls++
		return DeliveryResult{}, nil
	}
	if _, err := db.ExecuteDelivery(ctx, claim.Request, "wrong-claim", func() time.Time { return now }, apply); !errors.Is(err, ErrFencingConflict) {
		t.Fatalf("unfenced admitted replay error = %v, want fencing conflict", err)
	}
	if _, err := db.ExecuteDelivery(ctx, claim.Request, claim.ClaimToken, func() time.Time { return now }, apply); !errors.Is(err, ErrNoActiveDeliveryPlan) {
		t.Fatalf("inactive admitted replay error = %v, want no active delivery plan", err)
	}
	if applyCalls != 0 {
		t.Fatalf("inactive admitted replay apply calls = %d, want 0", applyCalls)
	}
}

func TestWorkflowInboxProjectionFencesCompleteActivePlanSet(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := "owner/repository"
	now := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)
	firstVersionID := activateWorkflowInboxPlanAt(t, ctx, db, repository, 1, 1, 2, 2)
	secondVersionID := activateWorkflowInboxPlanAt(t, ctx, db, repository, 11, 11, 12, 12)
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureWorkflowQuestionTx(ctx, tx, repository, firstVersionID, 2, "first", "first question", now); err != nil {
		t.Fatal(err)
	}
	if err := ensureWorkflowQuestionTx(ctx, tx, repository, secondVersionID, 12, "second", "second question", now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	queued, err := db.QueueWorkflowInboxProjection(ctx, repository, now)
	if err != nil {
		t.Fatal(err)
	}
	expectedVersions := []string{firstVersionID, secondVersionID}
	slices.Sort(expectedVersions)
	if queued.Request.InboxPlanVersionID != "" || !slices.Equal(queued.Request.InboxPlanVersionIDs, expectedVersions) {
		t.Fatalf("Inbox plan fence = %q/%v, want complete set %v", queued.Request.InboxPlanVersionID, queued.Request.InboxPlanVersionIDs, expectedVersions)
	}
	claim, err := db.ClaimDeliveryOutbox(ctx, queued.IdempotencyKey, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, "INSERT INTO completed_plan_versions(version_id, completed_at) VALUES (?, ?)", secondVersionID, formatTimestamp(now.Add(time.Second))); err != nil {
		t.Fatal(err)
	}
	applyCalls := 0
	if _, err := db.ExecuteDelivery(ctx, claim.Request, claim.ClaimToken, func() time.Time { return now.Add(time.Second) }, func(context.Context, DeliveryRequest) (DeliveryResult, error) {
		applyCalls++
		return DeliveryResult{}, nil
	}); !errors.Is(err, ErrNoActiveDeliveryPlan) {
		t.Fatalf("changed active set dispatch error = %v, want no active delivery plan", err)
	}
	if applyCalls != 0 {
		t.Fatalf("changed active set apply calls = %d, want 0", applyCalls)
	}
	questions, _, activeVersionIDs, err := db.WorkflowInboxProjection(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	if len(activeVersionIDs) != 1 || activeVersionIDs[0] != firstVersionID || len(questions) != 1 || questions[0].VersionID != firstVersionID {
		t.Fatalf("active Inbox projection versions=%v questions=%#v", activeVersionIDs, questions)
	}
}

func TestWorkflowInboxClaimsSerializeRepositoryGenerations(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := "owner/repository"
	now := time.Date(2026, 8, 7, 0, 0, 0, 0, time.UTC)
	versionID := activateWorkflowInboxPlanAt(t, ctx, db, repository, 1, 1, 2, 2)
	first, err := db.QueueWorkflowInboxProjection(ctx, repository, now)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureWorkflowQuestionTx(ctx, tx, repository, versionID, 2, "needs_attention", "retry the ticket", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	second, err := db.QueueWorkflowInboxProjection(ctx, repository, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if first.IdempotencyKey == second.IdempotencyKey || first.Request.InboxProjectionGeneration >= second.Request.InboxProjectionGeneration {
		t.Fatalf("projection generations = %#v then %#v", first.Request, second.Request)
	}
	t.Logf("queued Workflow Inbox generations in order: %d then %d", first.Request.InboxProjectionGeneration, second.Request.InboxProjectionGeneration)
	firstClaim, err := db.ClaimDeliveryOutbox(ctx, first.IdempotencyKey, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimDeliveryOutbox(ctx, second.IdempotencyKey, now.Add(2*time.Second)); !errors.Is(err, ErrDeliveryInProgress) {
		t.Fatalf("concurrent Inbox generation claim = %v, want in progress", err)
	}
	t.Logf("generation %d remained blocked while generation %d was in progress", second.Request.InboxProjectionGeneration, first.Request.InboxProjectionGeneration)
	if err := db.FinishDeliveryOutbox(ctx, first.IdempotencyKey, firstClaim.ClaimToken, OutboxSucceeded, "", now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimDeliveryOutbox(ctx, second.IdempotencyKey, now.Add(4*time.Second)); err != nil {
		t.Fatalf("next Inbox generation claim = %v", err)
	}
	t.Logf("generation %d became claimable only after generation %d completed", second.Request.InboxProjectionGeneration, first.Request.InboxProjectionGeneration)
}

func TestWorkflowInboxClaimsFenceOlderUncertainGeneration(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := "owner/repository"
	now := time.Date(2026, 8, 7, 4, 0, 0, 0, time.UTC)
	versionID := activateWorkflowInboxPlanAt(t, ctx, db, repository, 1, 1, 2, 2)
	first, err := db.QueueWorkflowInboxProjection(ctx, repository, now)
	if err != nil {
		t.Fatal(err)
	}
	firstClaim, err := db.ClaimDeliveryOutbox(ctx, first.IdempotencyKey, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RequeueDeliveryOutboxClaim(ctx, first.IdempotencyKey, firstClaim.ClaimToken, "remote outcome unknown", true, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureWorkflowQuestionTx(ctx, tx, repository, versionID, 2, "needs_attention", "retry the ticket", now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	second, err := db.QueueWorkflowInboxProjection(ctx, repository, now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if first.Request.InboxProjectionGeneration >= second.Request.InboxProjectionGeneration {
		t.Fatalf("Inbox generations = %d then %d", first.Request.InboxProjectionGeneration, second.Request.InboxProjectionGeneration)
	}
	if _, err := db.ClaimDeliveryOutbox(ctx, second.IdempotencyKey, now.Add(4*time.Second)); !errors.Is(err, ErrInboxDeliveryPending) {
		t.Fatalf("newer Inbox claim = %v, want pending", err)
	}
}

func TestWorkflowInboxUncertaintyExhaustsAndFencesUntilRecovery(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := "owner/repository"
	now := time.Date(2026, 8, 7, 4, 30, 0, 0, time.UTC)
	versionID := activateWorkflowInboxPlanAt(t, ctx, db, repository, 1, 1, 2, 2)
	queued, err := db.QueueWorkflowInboxProjection(ctx, repository, now)
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= maxDeliveryAttempts; attempt++ {
		claim, claimErr := db.ClaimDeliveryOutbox(ctx, queued.IdempotencyKey, now)
		if claimErr != nil {
			t.Fatalf("uncertain Inbox claim %d = %v", attempt, claimErr)
		}
		if err := db.RequeueDeliveryOutboxClaim(ctx, queued.IdempotencyKey, claim.ClaimToken, "observation unavailable", true, now); err != nil {
			t.Fatalf("uncertain Inbox requeue %d = %v", attempt, err)
		}
		now = now.Add(4 * time.Second)
	}
	exhausted, err := db.DeliveryOutbox(ctx, queued.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if exhausted.State != OutboxRejected || !exhausted.Uncertain || exhausted.Attempts != maxDeliveryAttempts {
		t.Fatalf("exhausted uncertain Inbox = %#v", exhausted)
	}
	t.Logf("uncertain generation %d exhausted after %d observations and exposed recovery key %s", queued.Request.InboxProjectionGeneration, exhausted.Attempts, queued.IdempotencyKey)
	if !strings.Contains(exhausted.LastError, queued.IdempotencyKey) || !strings.Contains(exhausted.LastError, "workflow recover-inbox-delivery") {
		t.Fatalf("exhausted uncertain Inbox recovery instructions = %q", exhausted.LastError)
	}
	questions, err := db.WorkflowInboxQuestions(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	foundRecoveryKey := false
	for _, question := range questions {
		foundRecoveryKey = foundRecoveryKey || strings.Contains(question.Prompt, queued.IdempotencyKey)
	}
	if !foundRecoveryKey {
		t.Fatalf("Needs Attention questions omit delivery key %q: %#v", queued.IdempotencyKey, questions)
	}
	projection, err := db.PlanProjectionAt(ctx, versionID, now)
	if err != nil {
		t.Fatal(err)
	}
	if projection.Tickets[0].State != "Needs Attention" {
		t.Fatalf("projection after exhausted Inbox reconciliation = %#v", projection)
	}
	newer, err := db.QueueWorkflowInboxProjection(ctx, repository, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimDeliveryOutbox(ctx, newer.IdempotencyKey, now); !errors.Is(err, ErrInboxDeliveryPending) {
		t.Fatalf("newer Inbox claim while exhausted generation is unresolved = %v", err)
	}
	t.Logf("newer generation %d stayed fenced behind unresolved generation %d", newer.Request.InboxProjectionGeneration, queued.Request.InboxProjectionGeneration)
	if _, err := db.db.ExecContext(ctx, `DELETE FROM delivery_outbox WHERE idempotency_key = ?`, newer.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	recoveryQuestionID, err := db.UncertainInboxDeliveryRecoveryQuestionID(ctx, repository, queued.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecoverUncertainInboxDelivery(ctx, repository, queued.IdempotencyKey, "unbound-question", "retry", now); !errors.Is(err, ErrInvalidClaim) {
		t.Fatalf("unbound Inbox recovery = %v, want invalid claim", err)
	}
	recoveryProjection, err := db.AnswerWorkflowQuestionAndQueueInboxProjection(ctx, repository, recoveryQuestionID, "retry", now)
	if err != nil {
		t.Fatal(err)
	}
	if recoveryProjection.State != OutboxPending || recoveryProjection.Request.InboxProjectionGeneration <= queued.Request.InboxProjectionGeneration {
		t.Fatalf("recovery projection = %#v", recoveryProjection)
	}
	t.Logf("operator answered recovery question %s; queued current generation %d behind reconciliation", recoveryQuestionID, recoveryProjection.Request.InboxProjectionGeneration)
	claim, err := db.ClaimDeliveryOutbox(ctx, queued.IdempotencyKey, now)
	if err != nil {
		t.Fatalf("recovered uncertain Inbox claim = %v", err)
	}
	if !claim.ReconcileOnly || !claim.Uncertain || claim.Attempts != 1 {
		t.Fatalf("recovered uncertain Inbox = %#v", claim)
	}
	t.Logf("recovery resumed generation %d in reconcile-only mode before the queued current generation", claim.Request.InboxProjectionGeneration)
	if err := db.RequeueDeliveryOutboxClaim(ctx, queued.IdempotencyKey, claim.ClaimToken, "observation still unavailable", true, now); err != nil {
		t.Fatal(err)
	}
	for attempt := 2; attempt <= maxDeliveryAttempts; attempt++ {
		now = now.Add(70 * time.Second)
		claim, err = db.ClaimDeliveryOutbox(ctx, queued.IdempotencyKey, now)
		if err != nil {
			t.Fatalf("second-cycle Inbox claim %d = %v", attempt, err)
		}
		if err := db.RequeueDeliveryOutboxClaim(ctx, queued.IdempotencyKey, claim.ClaimToken, "observation still unavailable", true, now); err != nil {
			t.Fatalf("second-cycle Inbox requeue %d = %v", attempt, err)
		}
	}
	secondQuestionID, err := db.UncertainInboxDeliveryRecoveryQuestionID(ctx, repository, queued.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if secondQuestionID == recoveryQuestionID {
		t.Fatalf("repeated terminal cycle reused recovery question %q", recoveryQuestionID)
	}
	if _, err := db.RecoverUncertainInboxDelivery(ctx, repository, queued.IdempotencyKey, recoveryQuestionID, "retry", now); !errors.Is(err, ErrInvalidClaim) {
		t.Fatalf("consumed Inbox recovery question reused = %v, want invalid claim", err)
	}
	if _, err := db.RecoverUncertainInboxDelivery(ctx, repository, queued.IdempotencyKey, secondQuestionID, "retry", now); err != nil {
		t.Fatalf("second-cycle Inbox recovery = %v", err)
	}
	var authorizations int
	if err := db.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM delivery_audits WHERE idempotency_key = ? AND decision = 'recovery_authorized'`, queued.IdempotencyKey).Scan(&authorizations); err != nil {
		t.Fatal(err)
	}
	if authorizations != 2 {
		t.Fatalf("recovery authorizations = %d, want 2 question-bound decisions", authorizations)
	}
	t.Logf("a repeated terminal cycle required a fresh question; recorded question-bound authorizations=%d", authorizations)
}

func TestUncertainInboxRecoveryPreservesCompletePlanProvenance(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := "owner/repository"
	now := time.Date(2026, 8, 7, 4, 45, 0, 0, time.UTC)
	firstVersionID := activateWorkflowInboxPlanAt(t, ctx, db, repository, 1, 1, 2, 2)
	secondVersionID := activateWorkflowInboxPlanAt(t, ctx, db, repository, 11, 11, 12, 12)
	queued, err := db.QueueWorkflowInboxProjection(ctx, repository, now)
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < maxDeliveryAttempts; attempt++ {
		claim, err := db.ClaimDeliveryOutbox(ctx, queued.IdempotencyKey, now)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.RequeueDeliveryOutboxClaim(ctx, queued.IdempotencyKey, claim.ClaimToken, "observation unavailable", true, now); err != nil {
			t.Fatal(err)
		}
		now = now.Add(4 * time.Second)
	}
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.cancelPlanTx(ctx, tx, firstVersionID, now); err != nil {
		t.Fatal(err)
	}
	if err := db.cancelPlanTx(ctx, tx, secondVersionID, now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	questions, _, versionIDs, err := db.WorkflowInboxProjectionState(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	expectedVersions := []string{firstVersionID, secondVersionID}
	slices.Sort(expectedVersions)
	if !slices.Equal(versionIDs, expectedVersions) {
		t.Fatalf("recovery provenance versions = %v, want %v", versionIDs, expectedVersions)
	}
	if len(questions) != 1 || questions[0].Kind != "inbox_delivery_recovery" || questions[0].RootNumber != 0 || !slices.Equal(questions[0].PlanNumbers, []int64{1, 11}) {
		t.Fatalf("repository recovery question = %#v", questions)
	}
}

func TestTerminalInboxRecoveryDoesNotQualifyStaleQuestions(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := "owner/repository"
	now := time.Date(2026, 8, 7, 5, 0, 0, 0, time.UTC)
	versionID := activateWorkflowInboxPlanAt(t, ctx, db, repository, 1, 1, 2, 2)
	queued, err := db.QueueWorkflowInboxProjection(ctx, repository, now)
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= maxDeliveryAttempts; attempt++ {
		claim, claimErr := db.ClaimDeliveryOutbox(ctx, queued.IdempotencyKey, now)
		if claimErr != nil {
			t.Fatalf("uncertain Inbox claim %d = %v", attempt, claimErr)
		}
		if err := db.RequeueDeliveryOutboxClaim(ctx, queued.IdempotencyKey, claim.ClaimToken, "observation unavailable", true, now); err != nil {
			t.Fatalf("uncertain Inbox requeue %d = %v", attempt, err)
		}
		now = now.Add(4 * time.Second)
	}
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureWorkflowQuestionTx(ctx, tx, repository, versionID, 0, "poll_failure", "retry polling", now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	questions, err := db.WorkflowInboxQuestions(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	var recoveryID string
	var staleIDs []string
	for _, question := range questions {
		switch question.Kind {
		case "inbox_delivery_recovery":
			recoveryID = question.ID
		case "needs_attention", "poll_failure":
			staleIDs = append(staleIDs, question.ID)
		}
	}
	if recoveryID == "" || len(staleIDs) != 2 {
		t.Fatalf("pre-cancellation questions = %#v", questions)
	}
	tx, err = db.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.cancelPlanTx(ctx, tx, versionID, now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	questions, err = db.WorkflowInboxQuestions(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	if len(questions) != 1 || questions[0].ID != recoveryID {
		t.Fatalf("terminal Inbox questions = %#v", questions)
	}
	for _, questionID := range staleIDs {
		if err := db.AnswerWorkflowQuestion(ctx, repository, questionID, "retry", now.Add(time.Second)); !errors.Is(err, ErrNotFound) {
			t.Fatalf("terminal stale question %q answer = %v, want not found", questionID, err)
		}
		question, err := db.WorkflowQuestion(ctx, repository, questionID)
		if err != nil || question.State != "open" || question.Answer != "" {
			t.Fatalf("terminal stale question %q changed = %#v, %v", questionID, question, err)
		}
	}
	if _, err := db.AnswerWorkflowQuestionAndQueueInboxProjection(ctx, repository, recoveryID, "retry", now.Add(2*time.Second)); err != nil {
		t.Fatalf("terminal Inbox recovery answer = %v", err)
	}
}

func TestWorkflowInboxAdmissionPersistsOnlyActiveQuestions(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := "owner/repository"
	now := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)
	activeVersionID := activateWorkflowInboxPlanAt(t, ctx, db, repository, 1, 1, 2, 2)
	inactiveVersionID := activateWorkflowInboxPlanAt(t, ctx, db, repository, 11, 11, 12, 12)
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureWorkflowQuestionTx(ctx, tx, repository, activeVersionID, 2, "active", "active question", now); err != nil {
		t.Fatal(err)
	}
	if err := ensureWorkflowQuestionTx(ctx, tx, repository, inactiveVersionID, 12, "inactive", "inactive question", now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkTicketDelivered(ctx, inactiveVersionID, 12); err != nil {
		t.Fatal(err)
	}
	allQuestions, err := db.OpenWorkflowQuestions(ctx, repository, 0)
	if err != nil || len(allQuestions) != 2 {
		t.Fatalf("all questions = %#v, %v", allQuestions, err)
	}
	queued, err := db.EnqueueDelivery(ctx, DeliveryRequest{
		Operation: DeliveryProjectInbox, Repository: repository,
		WorkflowQuestions: []plan.WorkflowQuestion{{ID: allQuestions[0].ID}, {ID: allQuestions[1].ID}},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued.Request.WorkflowQuestions) != 1 || queued.Request.WorkflowQuestions[0].Finding != "active" {
		t.Fatalf("persisted Inbox projection = %#v", queued.Request.WorkflowQuestions)
	}
	if queued.Request.InboxProjectionVersion == "" {
		t.Fatal("persisted Inbox projection version is empty")
	}
}

func TestAnswerWorkflowQuestionQueuesInboxProjectionAtomically(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	activateWorkflowInboxPlan(t, ctx, db, "owner/repository")
	var versionID string
	if err := db.db.QueryRowContext(ctx, `SELECT current_version_id FROM plans WHERE repository = ?`, "owner/repository").Scan(&versionID); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC)
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureWorkflowQuestionTx(ctx, tx, "owner/repository", versionID, 2, "needs_attention", "retry the ticket", now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	questions, err := db.OpenWorkflowQuestions(ctx, "owner/repository", 0)
	if err != nil || len(questions) != 1 {
		t.Fatalf("questions = %#v, %v", questions, err)
	}
	outbox, err := db.AnswerWorkflowQuestionAndQueueInboxProjection(ctx, "owner/repository", questions[0].ID, "retry", now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	question, err := db.WorkflowQuestion(ctx, "owner/repository", questions[0].ID)
	if err != nil || question.State != "answered" || question.Answer != "retry" {
		t.Fatalf("question = %#v, %v", question, err)
	}
	if outbox.State != OutboxPending || outbox.Request.Operation != DeliveryProjectInbox || len(outbox.Request.WorkflowQuestions) != 0 || outbox.Request.InboxProjectionVersion == "" {
		t.Fatalf("outbox = %#v", outbox)
	}
}

func TestResumeGatewayWritesCommitsAfterPlanCompletes(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := "owner/repository"
	activateWorkflowInboxPlan(t, ctx, db, repository)
	now := time.Now().UTC()
	rotation, err := db.BeginGatewayCredentialRotation(ctx, "rotation", "credential unavailable", now)
	if err != nil {
		t.Fatal(err)
	}
	var versionID string
	if err := db.db.QueryRowContext(ctx, `SELECT current_version_id FROM plans WHERE repository = ?`, repository).Scan(&versionID); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkTicketDelivered(ctx, versionID, 2); err != nil {
		t.Fatal(err)
	}
	if err := db.ResumeGatewayWrites(ctx, rotation, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	paused, _, err := db.GatewayWritesPaused(ctx)
	if err != nil || paused {
		t.Fatalf("Gateway pause after resume = %t, %v", paused, err)
	}
}

func activateWorkflowInboxPlan(t *testing.T, ctx context.Context, db *Store, repository string) {
	t.Helper()
	activateWorkflowInboxPlanAt(t, ctx, db, repository, 1, 1, 2, 2)
}

func activateWorkflowInboxPlanAt(t *testing.T, ctx context.Context, db *Store, repository string, rootID, rootNumber, ticketID, ticketNumber int64) string {
	t.Helper()
	snapshot := plan.Snapshot{
		Repository: repository,
		Root:       plan.Issue{ID: rootID, Number: rootNumber, Labels: []string{plan.PlanLabel}},
		Children:   []plan.Issue{{ID: ticketID, Number: ticketNumber, Labels: []string{plan.TicketLabel}, State: "open"}},
	}
	fingerprint, err := snapshot.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	version, err := db.BeginActivation(ctx, snapshot, fingerprint, "workflow-inbox")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkActive(ctx, version.ID); err != nil {
		t.Fatal(err)
	}
	return version.ID
}
