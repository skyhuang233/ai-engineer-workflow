package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestGatewayCredentialPauseUsesOneDurableInboxItemAndResumes(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	first := time.Date(2026, 7, 31, 3, 0, 0, 0, time.UTC)
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
	if err := db.ResumeGatewayWrites(ctx, first.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	paused, _, err = db.GatewayWritesPaused(ctx)
	item, itemErr := db.WorkflowInboxItem(ctx, GatewayCredentialInboxKey)
	if err != nil || itemErr != nil || paused || item.State != "resolved" {
		t.Fatalf("resumed pause=%t item=%#v errors=%v/%v", paused, item, err, itemErr)
	}
}

func TestGatewayPauseFencesNewDispatchAdmissionsAndWaitsForExistingOnes(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
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
