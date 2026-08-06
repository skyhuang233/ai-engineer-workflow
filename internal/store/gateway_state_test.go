package store

import (
	"context"
	"errors"
	"path/filepath"
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
}

func TestAdmittedWorkflowInboxReplayUsesPersistedClaimFence(t *testing.T) {
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
	if _, err := db.ExecuteDelivery(ctx, claim.Request, claim.ClaimToken, func() time.Time { return now }, apply); err != nil {
		t.Fatalf("fenced admitted replay = %v", err)
	}
	if applyCalls != 1 {
		t.Fatalf("admitted replay apply calls = %d, want 1", applyCalls)
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
	if outbox.State != OutboxPending || outbox.Request.Operation != DeliveryProjectInbox || outbox.Request.WorkflowQuestions != nil || outbox.Request.InboxProjectionVersion == "" {
		t.Fatalf("outbox = %#v", outbox)
	}
}

func activateWorkflowInboxPlan(t *testing.T, ctx context.Context, db *Store, repository string) {
	t.Helper()
	snapshot := plan.Snapshot{
		Repository: repository,
		Root:       plan.Issue{ID: 1, Number: 1, Labels: []string{plan.PlanLabel}},
		Children:   []plan.Issue{{ID: 2, Number: 2, Labels: []string{plan.TicketLabel}, State: "open"}},
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
}
