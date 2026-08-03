package delivery_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skyhuang233/workflow/internal/delivery"
	"github.com/skyhuang233/workflow/internal/plan"
	"github.com/skyhuang233/workflow/internal/store"
)

type fakeRemote struct {
	observations []delivery.Observation
	observeErrs  []error
	applyErr     error
	applyCalls   int
	observeCalls int
}

func (f *fakeRemote) Observe(context.Context, store.DeliveryRequest) (delivery.Observation, error) {
	f.observeCalls++
	if len(f.observeErrs) > 0 {
		err := f.observeErrs[0]
		f.observeErrs = f.observeErrs[1:]
		if err != nil {
			return delivery.Observation{}, err
		}
	}
	if len(f.observations) == 0 {
		return delivery.Observation{}, nil
	}
	observation := f.observations[0]
	f.observations = f.observations[1:]
	if observation.RemoteHead != "" {
		observation.RemoteExists = true
	}
	return observation, nil
}

type deadlineRemote struct {
	deadlineSeen bool
}

func (r *deadlineRemote) Observe(context.Context, store.DeliveryRequest) (delivery.Observation, error) {
	return delivery.Observation{RemoteHead: "base", RemoteExists: true}, nil
}

func (r *deadlineRemote) Apply(ctx context.Context, _ store.DeliveryRequest) (delivery.Observation, error) {
	_, r.deadlineSeen = ctx.Deadline()
	<-ctx.Done()
	return delivery.Observation{}, ctx.Err()
}

type blockingRemote struct {
	entered chan struct{}
	release chan struct{}
}

func (r *blockingRemote) Observe(context.Context, store.DeliveryRequest) (delivery.Observation, error) {
	return delivery.Observation{RemoteHead: "base", RemoteExists: true}, nil
}

func (r *blockingRemote) Apply(context.Context, store.DeliveryRequest) (delivery.Observation, error) {
	close(r.entered)
	<-r.release
	return delivery.Observation{Applied: true, RemoteHead: "accepted", RemoteExists: true}, nil
}

func TestOutboxCompletionIsFencedAndRetriesBecomeNeedsAttention(t *testing.T) {
	ctx := context.Background()
	db, claim := newAcceptedClaim(t, ctx)
	defer db.Close()
	queued, err := db.EnqueueDelivery(ctx, store.DeliveryRequest{
		Operation: store.DeliveryPushCandidate, RunID: claim.RunID, LeaseToken: claim.LeaseToken, LeaseGeneration: claim.LeaseGeneration,
		Repository: "owner/repo", Branch: "ticket-1", CommitSHA: "accepted", ExpectedRemoteHead: "base",
	}, time.Date(2026, 7, 31, 0, 3, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	first, err := db.ClaimDeliveryOutbox(ctx, queued.IdempotencyKey, time.Date(2026, 7, 31, 0, 3, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	second, err := db.ClaimDeliveryOutbox(ctx, queued.IdempotencyKey, time.Date(2026, 7, 31, 1, 3, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FinishDeliveryOutbox(ctx, queued.IdempotencyKey, second.ClaimToken, store.OutboxPending, "second failure", time.Date(2026, 7, 31, 1, 3, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishDeliveryOutbox(ctx, queued.IdempotencyKey, first.ClaimToken, store.OutboxSucceeded, "", time.Date(2026, 7, 31, 1, 3, 1, 0, time.UTC)); !errors.Is(err, store.ErrFencingConflict) {
		t.Fatalf("stale completion error = %v, want fencing conflict", err)
	}
	third, err := db.ClaimDeliveryOutbox(ctx, queued.IdempotencyKey, time.Date(2026, 7, 31, 1, 3, 2, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FinishDeliveryOutbox(ctx, queued.IdempotencyKey, third.ClaimToken, store.OutboxPending, "third failure", time.Date(2026, 7, 31, 1, 3, 2, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	fourth, err := db.ClaimDeliveryOutbox(ctx, queued.IdempotencyKey, time.Date(2026, 7, 31, 1, 3, 4, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FinishDeliveryOutbox(ctx, queued.IdempotencyKey, fourth.ClaimToken, store.OutboxPending, "fourth failure", time.Date(2026, 7, 31, 1, 3, 4, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	finished, err := db.DeliveryOutbox(ctx, queued.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if finished.State != store.OutboxRejected || !strings.Contains(finished.LastError, "retries exhausted") {
		t.Fatalf("exhausted outbox = %#v", finished)
	}
	projection, err := db.PlanProjectionAt(ctx, claim.VersionID, time.Date(2026, 7, 31, 0, 5, 3, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.Tickets) != 1 || projection.Tickets[0].State != "Needs Attention" {
		t.Fatalf("projection after retry exhaustion = %#v", projection)
	}
}

func (f *fakeRemote) Apply(context.Context, store.DeliveryRequest) (delivery.Observation, error) {
	f.applyCalls++
	if f.applyErr != nil {
		return delivery.Observation{}, f.applyErr
	}
	return delivery.Observation{Applied: true, PullRequestNumber: 17, RemoteHead: "accepted"}, nil
}

func TestGatewayUsesDurableOutboxAndReconcilesAnUncertainWrite(t *testing.T) {
	ctx := context.Background()
	db, claim := newAcceptedClaim(t, ctx)
	defer db.Close()
	remote := &fakeRemote{
		applyErr: errors.New("request timed out"),
		observations: []delivery.Observation{
			{RemoteHead: "base"},
			{Applied: true, RemoteHead: "accepted", PullRequestNumber: 17},
		},
	}
	gateway := delivery.Gateway{Store: db, Remote: remote, Now: func() time.Time { return time.Date(2026, 7, 31, 1, 0, 0, 0, time.UTC) }}
	queued, err := gateway.Submit(ctx, store.DeliveryRequest{
		Operation: store.DeliveryUpsertPR, RunID: claim.RunID, LeaseToken: claim.LeaseToken, LeaseGeneration: claim.LeaseGeneration,
		Repository: "owner/repo", Branch: "ticket-1", CommitSHA: "accepted", ExpectedRemoteHead: "base", Title: "ticket", Body: "evidence",
	})
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := gateway.Submit(ctx, queued.Request)
	if err != nil || duplicate.IdempotencyKey != queued.IdempotencyKey {
		t.Fatalf("duplicate = %#v, err = %v", duplicate, err)
	}
	if err := gateway.Dispatch(ctx, queued.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	outbox, err := db.DeliveryOutbox(ctx, queued.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if outbox.State != store.OutboxSucceeded || remote.observeCalls != 2 || remote.applyCalls != 1 {
		t.Fatalf("outbox = %#v, observes = %d, applies = %d", outbox, remote.observeCalls, remote.applyCalls)
	}
	retried, err := gateway.Submit(ctx, queued.Request)
	if err != nil || retried.IdempotencyKey != queued.IdempotencyKey || retried.ID != queued.ID {
		t.Fatalf("post-mapping retry = %#v, err = %v", retried, err)
	}
}

func TestGatewayPersistsUncertaintyAndAcceptsAppliedObservationBeforePreconditions(t *testing.T) {
	ctx := context.Background()
	db, claim := newAcceptedClaim(t, ctx)
	defer db.Close()
	now := time.Date(2026, 7, 31, 1, 0, 0, 0, time.UTC)
	remote := &fakeRemote{
		applyErr: errors.New("request timed out"),
		observations: []delivery.Observation{
			{RemoteHead: "base"},
			{Applied: true, RemoteHead: "accepted"},
		},
		observeErrs: []error{nil, errors.New("observation unavailable"), nil},
	}
	gateway := delivery.Gateway{Store: db, Remote: remote, Now: func() time.Time { return now }}
	queued, err := gateway.Submit(ctx, store.DeliveryRequest{
		Operation: store.DeliveryPushCandidate, RunID: claim.RunID, LeaseToken: claim.LeaseToken, LeaseGeneration: claim.LeaseGeneration,
		Repository: "owner/repo", Branch: "ticket-1", CommitSHA: "accepted", ExpectedRemoteHead: "base",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Dispatch(ctx, queued.IdempotencyKey); err == nil {
		t.Fatal("ambiguous write returned nil error")
	}
	uncertain, err := db.DeliveryOutbox(ctx, queued.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if uncertain.State != store.OutboxPending || !uncertain.Uncertain {
		t.Fatalf("uncertain outbox = %#v", uncertain)
	}
	now = now.Add(2 * time.Second)
	if err := gateway.Dispatch(ctx, queued.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	finished, err := db.DeliveryOutbox(ctx, queued.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if finished.State != store.OutboxSucceeded || finished.Uncertain || remote.applyCalls != 1 {
		t.Fatalf("reconciled outbox = %#v, apply calls = %d", finished, remote.applyCalls)
	}
}

func TestGatewayRejectsZombieCommandAfterLeaseReplacement(t *testing.T) {
	ctx := context.Background()
	db, claim := newAcceptedClaim(t, ctx)
	defer db.Close()
	remote := &fakeRemote{}
	gateway := delivery.Gateway{Store: db, Remote: remote, Now: func() time.Time { return time.Date(2026, 7, 31, 1, 0, 0, 0, time.UTC) }}
	request := store.DeliveryRequest{Operation: store.DeliveryPushCandidate, RunID: claim.RunID, LeaseToken: claim.LeaseToken, LeaseGeneration: claim.LeaseGeneration, Repository: "owner/repo", Branch: "ticket-1", CommitSHA: "accepted", ExpectedRemoteHead: "base"}
	if err := db.RecordRunFailure(ctx, store.RunFailure{RunID: claim.RunID, LeaseToken: claim.LeaseToken, DiagnosticsPath: "diagnostics", Error: "worker replaced", Now: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	queued, err := gateway.Submit(ctx, request)
	if err == nil || !errors.Is(err, store.ErrDeliveryRejected) || queued.IdempotencyKey != "" {
		t.Fatalf("zombie submit = %#v, err = %v", queued, err)
	}
	if remote.applyCalls != 0 {
		t.Fatal("zombie command reached remote")
	}
}

func TestGatewayAllowsFirstCandidatePushToExpectAbsentBranch(t *testing.T) {
	ctx := context.Background()
	db, claim := newAcceptedClaim(t, ctx)
	defer db.Close()
	remote := &fakeRemote{observations: []delivery.Observation{{}}}
	gateway := delivery.Gateway{Store: db, Remote: remote, Now: func() time.Time { return time.Date(2026, 7, 31, 1, 0, 0, 0, time.UTC) }}
	queued, err := gateway.Submit(ctx, store.DeliveryRequest{
		Operation: store.DeliveryPushCandidate, RunID: claim.RunID, LeaseToken: claim.LeaseToken, LeaseGeneration: claim.LeaseGeneration,
		Repository: "owner/repo", Branch: "ticket-1", CommitSHA: "accepted", ExpectRemoteAbsent: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Dispatch(ctx, queued.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	if remote.applyCalls != 1 {
		t.Fatalf("first push apply calls = %d", remote.applyCalls)
	}
}

func TestGatewayAllowsAcceptedCandidateDeliveryBeforeLeaseDeadline(t *testing.T) {
	ctx := context.Background()
	db, claim := newPublishedCandidate(t, ctx)
	defer db.Close()
	remote := &fakeRemote{observations: []delivery.Observation{{RemoteHead: "base"}}}
	gateway := delivery.Gateway{Store: db, Remote: remote, Now: func() time.Time { return time.Date(2026, 7, 31, 0, 1, 30, 0, time.UTC) }}
	queued, err := gateway.Submit(ctx, store.DeliveryRequest{
		Operation: store.DeliveryPushCandidate, RunID: claim.RunID, LeaseToken: claim.LeaseToken, LeaseGeneration: claim.LeaseGeneration,
		Repository: "owner/repo", Branch: "ticket-1", CommitSHA: "accepted", ExpectedRemoteHead: "base",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Dispatch(ctx, queued.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	if remote.applyCalls != 1 {
		t.Fatalf("accepted candidate was not delivered: applies=%d", remote.applyCalls)
	}
}

func TestGatewayRejectsUnstructuredPlanBodyReplacement(t *testing.T) {
	ctx := context.Background()
	db, claim := newAcceptedClaim(t, ctx)
	defer db.Close()
	gateway := delivery.Gateway{Store: db, Remote: &fakeRemote{}, Now: func() time.Time { return time.Date(2026, 7, 31, 1, 0, 0, 0, time.UTC) }}
	_, err := gateway.Submit(ctx, store.DeliveryRequest{
		Operation: store.DeliveryProjectPlan, RunID: claim.RunID, LeaseToken: claim.LeaseToken, LeaseGeneration: claim.LeaseGeneration,
		Repository: "owner/repo", RootNumber: 10, Body: "replace the human specification",
	})
	if err == nil || !errors.Is(err, store.ErrDeliveryRejected) {
		t.Fatalf("unstructured plan replacement error = %v, want ErrDeliveryRejected", err)
	}
}

func TestGatewayRejectsRemoteHeadDriftBeforeExternalWrite(t *testing.T) {
	ctx := context.Background()
	db, claim := newAcceptedClaim(t, ctx)
	defer db.Close()
	remote := &fakeRemote{observations: []delivery.Observation{{RemoteHead: "someone-else"}}}
	gateway := delivery.Gateway{Store: db, Remote: remote, Now: func() time.Time { return time.Date(2026, 7, 31, 1, 0, 0, 0, time.UTC) }}
	queued, err := gateway.Submit(ctx, store.DeliveryRequest{
		Operation: store.DeliveryPushCandidate, RunID: claim.RunID, LeaseToken: claim.LeaseToken, LeaseGeneration: claim.LeaseGeneration,
		Repository: "owner/repo", Branch: "ticket-1", CommitSHA: "accepted", ExpectedRemoteHead: "base",
	})
	if err != nil {
		t.Fatal(err)
	}
	err = gateway.Dispatch(ctx, queued.IdempotencyKey)
	if err == nil || !errors.Is(err, store.ErrDeliveryRejected) {
		t.Fatalf("head drift error = %v, want ErrDeliveryRejected", err)
	}
	if remote.applyCalls != 0 {
		t.Fatal("remote write occurred after head drift")
	}
	outbox, err := db.DeliveryOutbox(ctx, queued.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if outbox.State != store.OutboxRejected {
		t.Fatalf("outbox state = %q, want rejected", outbox.State)
	}
	audits, err := db.DeliveryAudits(ctx, queued.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if len(audits) < 2 || audits[len(audits)-1].Decision != "rejected" {
		t.Fatalf("audits = %#v", audits)
	}
}

func TestLeaseTakeoverCannotCommitAcrossInflightExternalWrite(t *testing.T) {
	ctx := context.Background()
	db, claim := newAcceptedClaim(t, ctx)
	defer db.Close()
	remote := &blockingRemote{entered: make(chan struct{}), release: make(chan struct{})}
	gateway := delivery.Gateway{Store: db, Remote: remote, Now: func() time.Time { return time.Date(2026, 7, 31, 1, 0, 0, 0, time.UTC) }}
	queued, err := gateway.Submit(ctx, store.DeliveryRequest{
		Operation: store.DeliveryPushCandidate, RunID: claim.RunID, LeaseToken: claim.LeaseToken, LeaseGeneration: claim.LeaseGeneration,
		Repository: "owner/repo", Branch: "ticket-1", CommitSHA: "accepted", ExpectedRemoteHead: "base",
	})
	if err != nil {
		t.Fatal(err)
	}
	dispatched := make(chan error, 1)
	go func() { dispatched <- gateway.Dispatch(ctx, queued.IdempotencyKey) }()
	<-remote.entered
	readComplete := make(chan error, 1)
	go func() {
		_, err := db.DeliveryOutbox(ctx, queued.IdempotencyKey)
		readComplete <- err
	}()
	select {
	case err := <-readComplete:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(50 * time.Millisecond):
		t.Fatal("external write held the SQLite transaction")
	}
	takeover := make(chan error, 1)
	go func() {
		takeover <- db.RecordRunFailure(ctx, store.RunFailure{RunID: claim.RunID, LeaseToken: claim.LeaseToken, DiagnosticsPath: "diagnostics", Error: "replace", Now: time.Date(2026, 7, 31, 1, 0, 1, 0, time.UTC)})
	}()
	select {
	case err := <-takeover:
		t.Fatalf("lease takeover completed before external write returned: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(remote.release)
	if err := <-dispatched; err != nil {
		t.Fatal(err)
	}
	if err := <-takeover; err != nil {
		t.Fatal(err)
	}
}

func TestGatewayBoundsExternalWriteByLeaseDeadline(t *testing.T) {
	ctx := context.Background()
	db, claim := newAcceptedClaim(t, ctx)
	defer db.Close()
	remote := &deadlineRemote{}
	gateway := delivery.Gateway{Store: db, Remote: remote, Now: func() time.Time {
		return time.Date(2026, 7, 31, 1, 1, 59, 950000000, time.UTC)
	}}
	queued, err := gateway.Submit(ctx, store.DeliveryRequest{
		Operation: store.DeliveryPushCandidate, RunID: claim.RunID, LeaseToken: claim.LeaseToken, LeaseGeneration: claim.LeaseGeneration,
		Repository: "owner/repo", Branch: "ticket-1", CommitSHA: "accepted", ExpectedRemoteHead: "base",
	})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := gateway.Dispatch(ctx, queued.IdempotencyKey); err == nil {
		t.Fatal("lease-bounded external write returned nil error")
	}
	if !remote.deadlineSeen || time.Since(started) > time.Second {
		t.Fatalf("external write was not bounded by lease deadline; deadline=%t elapsed=%s", remote.deadlineSeen, time.Since(started))
	}
}

func TestGatewayRejectsDeliveryWhenValidationConsumesLease(t *testing.T) {
	ctx := context.Background()
	db, claim := newAcceptedClaim(t, ctx)
	defer db.Close()
	clock := []time.Time{
		time.Date(2026, 7, 31, 1, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 31, 1, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 31, 1, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 31, 1, 3, 0, 0, time.UTC),
	}
	remote := &fakeRemote{}
	gateway := delivery.Gateway{Store: db, Remote: remote, Now: func() time.Time {
		value := clock[0]
		if len(clock) > 1 {
			clock = clock[1:]
		}
		return value
	}}
	queued, err := gateway.Submit(ctx, store.DeliveryRequest{
		Operation: store.DeliveryPushCandidate, RunID: claim.RunID, LeaseToken: claim.LeaseToken, LeaseGeneration: claim.LeaseGeneration,
		Repository: "owner/repo", Branch: "ticket-1", CommitSHA: "accepted", ExpectedRemoteHead: "base",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Dispatch(ctx, queued.IdempotencyKey); err == nil || !errors.Is(err, store.ErrDeliveryRejected) {
		t.Fatalf("expired delivery error = %v", err)
	}
	if remote.observeCalls != 0 || remote.applyCalls != 0 {
		t.Fatalf("delivery reached remote after lease expiry: observes=%d applies=%d", remote.observeCalls, remote.applyCalls)
	}
}

func TestOutboxProcessingLeaseCanBeReclaimedAfterRestart(t *testing.T) {
	ctx := context.Background()
	db, claim := newAcceptedClaim(t, ctx)
	defer db.Close()
	queued, err := db.EnqueueDelivery(ctx, store.DeliveryRequest{
		Operation: store.DeliveryPushCandidate, RunID: claim.RunID, LeaseToken: claim.LeaseToken, LeaseGeneration: claim.LeaseGeneration,
		Repository: "owner/repo", Branch: "ticket-1", CommitSHA: "accepted", ExpectedRemoteHead: "base",
	}, time.Date(2026, 7, 31, 0, 3, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimDeliveryOutbox(ctx, queued.IdempotencyKey, time.Date(2026, 7, 31, 0, 3, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimDeliveryOutbox(ctx, queued.IdempotencyKey, time.Date(2026, 7, 31, 0, 3, 30, 0, time.UTC)); !errors.Is(err, store.ErrDeliveryInProgress) {
		t.Fatalf("concurrent claim error = %v, want ErrDeliveryInProgress", err)
	}
	reclaimed, err := db.ClaimDeliveryOutbox(ctx, queued.IdempotencyKey, time.Date(2026, 7, 31, 1, 3, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed.Attempts != 1 || reclaimed.State != store.OutboxProcessing || !reclaimed.ReconcileOnly {
		t.Fatalf("reclaimed outbox = %#v", reclaimed)
	}
}

func TestGatewayReconcilesExpiredUncertainWriteWithoutApplying(t *testing.T) {
	ctx := context.Background()
	db, claim := newAcceptedClaim(t, ctx)
	defer db.Close()
	queued, err := db.EnqueueDelivery(ctx, store.DeliveryRequest{
		Operation: store.DeliveryPushCandidate, RunID: claim.RunID, LeaseToken: claim.LeaseToken, LeaseGeneration: claim.LeaseGeneration,
		Repository: "owner/repo", Branch: "ticket-1", CommitSHA: "accepted", ExpectedRemoteHead: "base",
	}, time.Date(2026, 7, 31, 1, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimDeliveryOutbox(ctx, queued.IdempotencyKey, time.Date(2026, 7, 31, 1, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	remote := &fakeRemote{observations: []delivery.Observation{{Applied: true, RemoteHead: "accepted"}}}
	gateway := delivery.Gateway{Store: db, Remote: remote, Now: func() time.Time { return time.Date(2026, 7, 31, 1, 3, 0, 0, time.UTC) }}
	if err := gateway.Dispatch(ctx, queued.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	outbox, err := db.DeliveryOutbox(ctx, queued.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if outbox.State != store.OutboxSucceeded || remote.applyCalls != 0 || remote.observeCalls != 1 {
		t.Fatalf("reconciled outbox = %#v, applies=%d observes=%d", outbox, remote.applyCalls, remote.observeCalls)
	}
}

func TestReplacedLeaseCannotUpdateMappedPROrReplyWithEvidence(t *testing.T) {
	ctx := context.Background()
	db, claim := newAcceptedClaim(t, ctx)
	defer db.Close()
	remote := &fakeRemote{observations: []delivery.Observation{{RemoteHead: "base"}}}
	gateway := delivery.Gateway{Store: db, Remote: remote, Now: func() time.Time { return time.Date(2026, 7, 31, 1, 0, 0, 0, time.UTC) }}
	initial, err := gateway.Submit(ctx, store.DeliveryRequest{
		Operation: store.DeliveryUpsertPR, RunID: claim.RunID, LeaseToken: claim.LeaseToken, LeaseGeneration: claim.LeaseGeneration,
		Repository: "owner/repo", Branch: "ticket-1", CommitSHA: "accepted", ExpectedRemoteHead: "base", Title: "ticket", Body: "first",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Dispatch(ctx, initial.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	update, err := gateway.Submit(ctx, store.DeliveryRequest{
		Operation: store.DeliveryUpsertPR, RunID: claim.RunID, LeaseToken: claim.LeaseToken, LeaseGeneration: claim.LeaseGeneration,
		Repository: "owner/repo", Branch: "ticket-1", CommitSHA: "accepted", ExpectedRemoteHead: "base", Title: "ticket revised", Body: "second",
	})
	if err != nil {
		t.Fatal(err)
	}
	reply, err := gateway.Submit(ctx, store.DeliveryRequest{
		Operation: store.DeliveryReplyEvidence, RunID: claim.RunID, LeaseToken: claim.LeaseToken, LeaseGeneration: claim.LeaseGeneration,
		Repository: "owner/repo", Branch: "ticket-1", Evidence: "evidence",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RecordRunFailure(ctx, store.RunFailure{RunID: claim.RunID, LeaseToken: claim.LeaseToken, DiagnosticsPath: "diagnostics", Error: "lease replaced", Now: time.Date(2026, 7, 31, 1, 1, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimReady(ctx, store.ClaimRequest{VersionID: claim.VersionID, TicketID: claim.TicketID, Owner: "replacement", MaxParallelRuns: 1, LeaseTTL: time.Hour, Now: time.Date(2026, 7, 31, 1, 2, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	applyCalls := remote.applyCalls
	for _, key := range []string{update.IdempotencyKey, reply.IdempotencyKey} {
		if err := gateway.Dispatch(ctx, key); err == nil || !errors.Is(err, store.ErrDeliveryRejected) {
			t.Fatalf("zombie dispatch %q error = %v", key, err)
		}
	}
	if remote.applyCalls != applyCalls {
		t.Fatalf("zombie PR/reply reached remote: before=%d after=%d", applyCalls, remote.applyCalls)
	}
}

func newAcceptedClaim(t *testing.T, ctx context.Context) (*store.Store, store.TicketClaim) {
	return newCandidateClaim(t, ctx, true)
}

func newPublishedCandidate(t *testing.T, ctx context.Context) (*store.Store, store.TicketClaim) {
	return newCandidateClaim(t, ctx, false)
}

func newCandidateClaim(t *testing.T, ctx context.Context, renewLease bool) (*store.Store, store.TicketClaim) {
	t.Helper()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	snapshot := plan.Snapshot{Repository: "owner/repo", Root: plan.Issue{ID: 100, Number: 10, Labels: []string{plan.PlanLabel}}, Children: []plan.Issue{{ID: 1, Number: 11, Title: "first", Labels: []string{plan.TicketLabel}, State: "open"}}}
	fingerprint, err := snapshot.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	version, err := db.BeginActivation(ctx, snapshot, fingerprint, "source")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkActive(ctx, version.ID); err != nil {
		t.Fatal(err)
	}
	claim, err := db.ClaimReady(ctx, store.ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent", MaxParallelRuns: 1, LeaseTTL: time.Hour, Now: time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.BindAgent(ctx, store.AgentBinding{SessionID: claim.SessionID, AgentIdentity: "agent", WorkspacePath: "workspace", CodexStatePath: "codex", Branch: "ticket-1"}); err != nil {
		t.Fatal(err)
	}
	if err := db.AcceptCandidate(ctx, store.CandidateRevision{RunID: claim.RunID, LeaseToken: claim.LeaseToken, CodexSessionID: "codex", CommitSHA: "accepted", StructuredOutput: []byte(`{"result":"ok"}`), Now: time.Date(2026, 7, 31, 0, 1, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	if renewLease {
		claim, err = db.ClaimReady(ctx, store.ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent", MaxParallelRuns: 1, LeaseTTL: time.Hour, Now: time.Date(2026, 7, 31, 0, 2, 0, 0, time.UTC)})
		if err != nil {
			t.Fatal(err)
		}
	}
	return db, claim
}
