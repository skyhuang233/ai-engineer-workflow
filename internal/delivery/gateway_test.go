package delivery_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/skyhuang233/workflow/internal/delivery"
	"github.com/skyhuang233/workflow/internal/plan"
	"github.com/skyhuang233/workflow/internal/store"
)

type fakeRemote struct {
	observations []delivery.Observation
	applyErr     error
	applyCalls   int
	observeCalls int
}

func (f *fakeRemote) Observe(context.Context, store.DeliveryRequest) (delivery.Observation, error) {
	f.observeCalls++
	if len(f.observations) == 0 {
		return delivery.Observation{}, nil
	}
	observation := f.observations[0]
	f.observations = f.observations[1:]
	return observation, nil
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
	reclaimed, err := db.ClaimDeliveryOutbox(ctx, queued.IdempotencyKey, time.Date(2026, 7, 31, 0, 5, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed.Attempts != 2 || reclaimed.State != store.OutboxProcessing {
		t.Fatalf("reclaimed outbox = %#v", reclaimed)
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
	// A published candidate is accepted before the delivery command is sent;
	// create a fresh run to model the delivery controller's active lease.
	claim, err = db.ClaimReady(ctx, store.ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent", MaxParallelRuns: 1, LeaseTTL: time.Hour, Now: time.Date(2026, 7, 31, 0, 2, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	return db, claim
}
