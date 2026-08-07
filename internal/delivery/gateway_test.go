package delivery_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skyhuang233/workflow/internal/delivery"
	githubapi "github.com/skyhuang233/workflow/internal/github"
	"github.com/skyhuang233/workflow/internal/plan"
	"github.com/skyhuang233/workflow/internal/store"
)

type fakeRemote struct {
	observations  []delivery.Observation
	observeErrs   []error
	applyErr      error
	applyCalls    int
	observeCalls  int
	requests      []store.DeliveryRequest
	credentialErr error
}

type completionFailingStore struct {
	*store.Store
	err error
}

type enqueueFailingStore struct {
	*store.Store
	err error
}

func (s enqueueFailingStore) EnqueueDelivery(context.Context, store.DeliveryRequest, time.Time) (store.DeliveryOutbox, error) {
	return store.DeliveryOutbox{}, s.err
}

func (s completionFailingStore) CompleteDeliveryOutbox(context.Context, string, string, store.DeliveryResult, time.Time) error {
	return s.err
}

func (f *fakeRemote) CredentialAvailable(context.Context) error { return f.credentialErr }

func (f *fakeRemote) Observe(_ context.Context, request store.DeliveryRequest) (delivery.Observation, error) {
	f.observeCalls++
	f.requests = append(f.requests, request)
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

func TestGatewayPreservesWorkflowQuestionContext(t *testing.T) {
	ctx := context.Background()
	db, claim := newAcceptedClaim(t, ctx)
	defer db.Close()
	now := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	if _, err := db.FreezePlanForClosedPullRequest(ctx, claim.VersionID, claim.TicketID, now); err != nil {
		t.Fatal(err)
	}
	questions, err := db.OpenWorkflowQuestions(ctx, "owner/repo", 0)
	if err != nil {
		t.Fatalf("questions = %#v, %v", questions, err)
	}
	var closedQuestion store.WorkflowQuestion
	for _, candidate := range questions {
		if candidate.Kind == "closed_unmerged_impact" {
			closedQuestion = candidate
			break
		}
	}
	if closedQuestion.ID == "" {
		t.Fatalf("closed question missing from %#v", questions)
	}
	remote := &fakeRemote{}
	gateway := delivery.Gateway{Store: db, Remote: remote, Now: func() time.Time { return now }}
	outbox, err := gateway.Submit(ctx, store.DeliveryRequest{Operation: store.DeliveryProjectInbox, Repository: "owner/repo"})
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Dispatch(ctx, outbox.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	if len(remote.requests) == 0 || len(remote.requests[0].WorkflowQuestions) != len(questions) {
		t.Fatalf("published requests = %#v", remote.requests)
	}
	var got plan.WorkflowQuestion
	for _, candidate := range remote.requests[0].WorkflowQuestions {
		if candidate.ID == closedQuestion.ID {
			got = candidate
			break
		}
	}
	want := closedQuestion
	if got.ID != want.ID || got.Prompt != want.Prompt || got.Repository != want.Repository || got.PlanNumber != want.RootNumber || got.TicketNumber != want.TicketNumber || got.PullRequest != want.PullRequest || got.Commit != want.Commit || got.Finding != want.Kind || got.Diagnostics != want.Diagnostics || got.Evidence != want.Evidence {
		t.Fatalf("published question = %#v, want %#v", got, want)
	}
}

func TestGatewayQueuesCredentialRecoveryInboxProjection(t *testing.T) {
	ctx := context.Background()
	db, _ := newAcceptedClaim(t, ctx)
	defer db.Close()
	now := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	if err := db.PauseGatewayWrites(ctx, "credential unavailable", now); err != nil {
		t.Fatal(err)
	}
	gateway := delivery.Gateway{Store: db, Now: func() time.Time { return now }}
	if err := gateway.QueueGatewayCredentialInboxProjections(ctx); err != nil {
		t.Fatal(err)
	}
	keys, err := db.DueDeliveryOutboxKeys(ctx, now, 1)
	if err != nil || len(keys) != 1 {
		t.Fatalf("credential recovery outbox keys = %#v, %v", keys, err)
	}
	outbox, err := db.DeliveryOutbox(ctx, keys[0])
	if err != nil || outbox.Request.Operation != store.DeliveryProjectInbox || len(outbox.Request.WorkflowQuestions) == 0 {
		t.Fatalf("credential recovery outbox = %#v, %v", outbox, err)
	}
}

func TestGatewayCredentialInboxQueueIgnoresInactiveRepositories(t *testing.T) {
	ctx := context.Background()
	db, claim := newAcceptedClaim(t, ctx)
	defer db.Close()
	now := time.Now().UTC()
	if err := db.PauseGatewayWrites(ctx, "credential unavailable", now); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkTicketDelivered(ctx, claim.VersionID, claim.TicketID); err != nil {
		t.Fatal(err)
	}
	gateway := delivery.Gateway{Store: db, Now: func() time.Time { return now.Add(time.Second) }}
	if err := gateway.QueueGatewayCredentialInboxProjections(ctx); err != nil {
		t.Fatalf("queue inactive credential Inbox = %v", err)
	}
	keys, err := db.DueDeliveryOutboxKeys(ctx, now.Add(time.Second), 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range keys {
		outbox, err := db.DeliveryOutbox(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if len(outbox.Request.WorkflowQuestions) != 0 {
			t.Fatalf("inactive credential Inbox projection = %#v", outbox.Request)
		}
	}
}

func TestGatewayStoreFailureHasStructuredHTTPClassification(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	gateway := delivery.Gateway{Store: enqueueFailingStore{Store: db, err: errors.New("sqlite is busy")}, Remote: &fakeRemote{}}
	server := httptest.NewServer(delivery.HTTPHandler(gateway, delivery.HTTPOptions{ControlPlaneToken: "control-token"}))
	defer server.Close()
	err = (delivery.HTTPProjector{URL: server.URL, ControlPlaneToken: "control-token", Client: &http.Client{Timeout: time.Second}}).ProjectWorkflowInbox(ctx, "owner/repo", nil)
	var httpErr *delivery.HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusInternalServerError || httpErr.Code != delivery.ErrorCodeRetryableStore || !httpErr.PollStoreFailure() {
		t.Fatalf("Gateway store HTTP error = %#v, %v", httpErr, err)
	}
}

func TestGatewayWritesPausedHasStructuredHTTPClassification(t *testing.T) {
	ctx := context.Background()
	db, _ := newAcceptedClaim(t, ctx)
	defer db.Close()
	now := time.Now().UTC()
	if err := db.PauseGatewayWrites(ctx, "rotate credential", now); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(delivery.HTTPHandler(delivery.Gateway{Store: db, Remote: &fakeRemote{}, Now: func() time.Time { return now }}, delivery.HTTPOptions{ControlPlaneToken: "control-token"}))
	defer server.Close()
	err := (delivery.HTTPProjector{URL: server.URL, ControlPlaneToken: "control-token", Client: &http.Client{Timeout: time.Second}}).ProjectWorkflowInbox(ctx, "owner/repo", nil)
	var httpErr *delivery.HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusServiceUnavailable || httpErr.Code != delivery.ErrorCodeGatewayWritesPaused || !httpErr.AuthenticationFailure() {
		t.Fatalf("paused Gateway HTTP error = %#v, %v", httpErr, err)
	}
}

func TestGatewayRateLimitHasStructuredHTTPRetryTime(t *testing.T) {
	ctx := context.Background()
	db, _ := newAcceptedClaim(t, ctx)
	defer db.Close()
	now := time.Date(2026, 8, 7, 0, 0, 0, 0, time.UTC)
	retryAt := now.Add(5 * time.Minute)
	remote := &fakeRemote{applyErr: &githubapi.APIError{StatusCode: http.StatusForbidden, RetryAt: retryAt}}
	server := httptest.NewServer(delivery.HTTPHandler(delivery.Gateway{Store: db, Remote: remote, Now: func() time.Time { return now }}, delivery.HTTPOptions{ControlPlaneToken: "control-token"}))
	defer server.Close()
	err := (delivery.HTTPProjector{URL: server.URL, ControlPlaneToken: "control-token", Client: &http.Client{Timeout: time.Second}}).ProjectWorkflowInbox(ctx, "owner/repo", nil)
	var httpErr *delivery.HTTPError
	if !errors.As(err, &httpErr) || !httpErr.RetryAtTime().Equal(retryAt) {
		t.Fatalf("Gateway rate-limit HTTP error = %#v, %v", httpErr, err)
	}
}

func TestGatewayResolvesCredentialRecoveryInboxAtDispatch(t *testing.T) {
	ctx := context.Background()
	db, _ := newAcceptedClaim(t, ctx)
	defer db.Close()
	now := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	if err := db.PauseGatewayWrites(ctx, "credential unavailable", now); err != nil {
		t.Fatal(err)
	}
	gateway := delivery.Gateway{Store: db, Remote: &fakeRemote{}, Now: func() time.Time { return now }}
	if err := gateway.QueueGatewayCredentialInboxProjections(ctx); err != nil {
		t.Fatal(err)
	}
	keys, err := db.DueDeliveryOutboxKeys(ctx, now, 1)
	if err != nil || len(keys) != 1 {
		t.Fatalf("credential recovery outbox keys = %#v, %v", keys, err)
	}
	rotation, err := db.BeginGatewayCredentialRotation(ctx, "operator", "replace credential", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ResumeGatewayWrites(ctx, rotation, now); err != nil {
		t.Fatal(err)
	}
	if err := gateway.Dispatch(ctx, keys[0]); err != nil {
		t.Fatalf("stale credential projection = %v", err)
	}
	if err := gateway.DispatchPending(ctx, 8); err != nil {
		t.Fatal(err)
	}
	remote := gateway.Remote.(*fakeRemote)
	for _, request := range remote.requests {
		if len(request.WorkflowQuestions) != 0 {
			t.Fatalf("dispatched stale credential recovery questions: %#v", remote.requests)
		}
	}
}

func TestGatewayNormalizesWorkflowInboxProjectionIntents(t *testing.T) {
	ctx := context.Background()
	db, claim := newAcceptedClaim(t, ctx)
	defer db.Close()
	now := time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC)
	if _, err := db.FreezePlanForClosedPullRequest(ctx, claim.VersionID, claim.TicketID, now); err != nil {
		t.Fatal(err)
	}
	current, err := db.QueueWorkflowInboxProjection(ctx, "owner/repo", now)
	if err != nil {
		t.Fatal(err)
	}
	stale, err := db.EnqueueDelivery(ctx, store.DeliveryRequest{
		Operation: store.DeliveryProjectInbox, Repository: "owner/repo",
		WorkflowQuestions: []plan.WorkflowQuestion{{ID: "stale"}}, InboxProjectionVersion: "superseded",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if stale.IdempotencyKey != current.IdempotencyKey || stale.Request.InboxProjectionVersion == "superseded" {
		t.Fatalf("normalized stale projection = %#v; current key=%q", stale, current.IdempotencyKey)
	}
	remote := &fakeRemote{}
	gateway := delivery.Gateway{Store: db, Remote: remote, Now: func() time.Time { return now }}
	if err := gateway.Dispatch(ctx, stale.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	if err := gateway.Dispatch(ctx, current.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	if len(remote.requests) != 1 || len(remote.requests[0].WorkflowQuestions) == 0 {
		t.Fatalf("current projection = %#v", remote.requests)
	}
	for _, question := range remote.requests[0].WorkflowQuestions {
		if question.ID == "stale" {
			t.Fatalf("current projection retained stale question: %#v", remote.requests)
		}
	}
}

func TestGatewayCompletesSupersededInboxGenerationWithoutRemoteWrite(t *testing.T) {
	ctx := context.Background()
	db, _ := newAcceptedClaim(t, ctx)
	defer db.Close()
	now := time.Date(2026, 8, 7, 2, 0, 0, 0, time.UTC)
	remote := &fakeRemote{}
	gateway := delivery.Gateway{Store: db, Remote: remote, Now: func() time.Time { return now }}
	first, err := gateway.Submit(ctx, store.DeliveryRequest{Operation: store.DeliveryProjectInbox, Repository: "owner/repo"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkRepositoryNeedsAttention(ctx, "owner/repo", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	second, err := gateway.Submit(ctx, store.DeliveryRequest{Operation: store.DeliveryProjectInbox, Repository: "owner/repo"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Request.InboxProjectionGeneration >= second.Request.InboxProjectionGeneration {
		t.Fatalf("Inbox generations = %d then %d", first.Request.InboxProjectionGeneration, second.Request.InboxProjectionGeneration)
	}
	if err := gateway.Dispatch(ctx, first.IdempotencyKey); err != nil {
		t.Fatalf("superseded Inbox dispatch = %v", err)
	}
	completed, err := db.DeliveryOutbox(ctx, first.IdempotencyKey)
	if err != nil || completed.State != store.OutboxSucceeded {
		t.Fatalf("superseded Inbox outbox = %#v, %v", completed, err)
	}
	if remote.observeCalls != 0 || remote.applyCalls != 0 {
		t.Fatalf("superseded Inbox reached remote: observe=%d apply=%d", remote.observeCalls, remote.applyCalls)
	}
}

func TestGatewayCompletesSupersededUncertainInboxBeforeNewerGeneration(t *testing.T) {
	ctx := context.Background()
	db, _ := newAcceptedClaim(t, ctx)
	defer db.Close()
	now := time.Date(2026, 8, 7, 2, 30, 0, 0, time.UTC)
	first, err := db.QueueWorkflowInboxProjection(ctx, "owner/repo", now)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := db.ClaimDeliveryOutbox(ctx, first.IdempotencyKey, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RequeueDeliveryOutboxClaim(ctx, first.IdempotencyKey, claim.ClaimToken, "remote outcome unknown", true, now); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkRepositoryNeedsAttention(ctx, "owner/repo", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	second, err := db.QueueWorkflowInboxProjection(ctx, "owner/repo", now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	remote := &fakeRemote{}
	now = now.Add(2 * time.Second)
	gateway := delivery.Gateway{Store: db, Remote: remote, Now: func() time.Time { return now }}
	if err := gateway.Dispatch(ctx, first.IdempotencyKey); err != nil {
		t.Fatalf("uncertain Inbox reconciliation = %v", err)
	}
	if remote.observeCalls != 0 || remote.applyCalls != 0 || len(remote.requests) != 0 {
		t.Fatalf("superseded Inbox reconciliation = observes %d applies %d requests %#v", remote.observeCalls, remote.applyCalls, remote.requests)
	}
	if err := gateway.Dispatch(ctx, second.IdempotencyKey); err != nil {
		t.Fatalf("newer Inbox dispatch = %v", err)
	}
	if remote.observeCalls != 1 || remote.applyCalls != 1 || len(remote.requests) != 1 || remote.requests[0].InboxProjectionGeneration != second.Request.InboxProjectionGeneration {
		t.Fatalf("authoritative Inbox dispatch = observes %d applies %d requests %#v", remote.observeCalls, remote.applyCalls, remote.requests)
	}
}

func TestGatewayAppliesCurrentUnobservedUncertainInbox(t *testing.T) {
	ctx := context.Background()
	db, _ := newAcceptedClaim(t, ctx)
	defer db.Close()
	now := time.Date(2026, 8, 7, 2, 45, 0, 0, time.UTC)
	queued, err := db.QueueWorkflowInboxProjection(ctx, "owner/repo", now)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := db.ClaimDeliveryOutbox(ctx, queued.IdempotencyKey, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RequeueDeliveryOutboxClaim(ctx, queued.IdempotencyKey, claim.ClaimToken, "remote outcome unknown", true, now); err != nil {
		t.Fatal(err)
	}
	remote := &fakeRemote{}
	gateway := delivery.Gateway{Store: db, Remote: remote, Now: func() time.Time { return now.Add(2 * time.Second) }}
	if err := gateway.Dispatch(ctx, queued.IdempotencyKey); err != nil {
		t.Fatalf("current uncertain Inbox dispatch = %v", err)
	}
	finished, err := db.DeliveryOutbox(ctx, queued.IdempotencyKey)
	if err != nil || finished.State != store.OutboxSucceeded || remote.observeCalls != 1 || remote.applyCalls != 1 {
		t.Fatalf("current uncertain Inbox = %#v, %v; observes=%d applies=%d", finished, err, remote.observeCalls, remote.applyCalls)
	}
}

func TestGatewayPreservesUncertainInboxAcrossCredentialPreflightFailures(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "transient", err: errors.New("credential store unavailable")},
		{name: "rejected", err: delivery.ErrGatewayCredentialRejected},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			db, _ := newAcceptedClaim(t, ctx)
			defer db.Close()
			now := time.Date(2026, 8, 7, 2, 50, 0, 0, time.UTC)
			queued, err := db.QueueWorkflowInboxProjection(ctx, "owner/repo", now)
			if err != nil {
				t.Fatal(err)
			}
			claim, err := db.ClaimDeliveryOutbox(ctx, queued.IdempotencyKey, now)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.RequeueDeliveryOutboxClaim(ctx, queued.IdempotencyKey, claim.ClaimToken, "remote outcome unknown", true, now); err != nil {
				t.Fatal(err)
			}
			remote := &fakeRemote{credentialErr: test.err}
			gateway := delivery.Gateway{Store: db, Remote: remote, Now: func() time.Time { return now.Add(2 * time.Second) }}
			if err := gateway.Dispatch(ctx, queued.IdempotencyKey); err == nil {
				t.Fatal("credential preflight failure returned nil")
			}
			outbox, err := db.DeliveryOutbox(ctx, queued.IdempotencyKey)
			if err != nil || outbox.State != store.OutboxPending || !outbox.Uncertain {
				t.Fatalf("uncertain Inbox after credential preflight = %#v, %v", outbox, err)
			}
			if remote.observeCalls != 0 || remote.applyCalls != 0 {
				t.Fatalf("credential preflight reached remote: observes=%d applies=%d", remote.observeCalls, remote.applyCalls)
			}
		})
	}
}

func TestHTTPProjectorAcceptsDurableInboxSerializationContention(t *testing.T) {
	ctx := context.Background()
	db, _ := newAcceptedClaim(t, ctx)
	defer db.Close()
	now := time.Date(2026, 8, 7, 3, 0, 0, 0, time.UTC)
	gateway := delivery.Gateway{Store: db, Remote: &fakeRemote{}, Now: func() time.Time { return now }}
	first, err := gateway.Submit(ctx, store.DeliveryRequest{Operation: store.DeliveryProjectInbox, Repository: "owner/repo"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkRepositoryNeedsAttention(ctx, "owner/repo", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := gateway.Submit(ctx, store.DeliveryRequest{Operation: store.DeliveryProjectInbox, Repository: "owner/repo"}); err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureGatewayDispatcher(ctx, "legacy-gateway-dispatcher", now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimDeliveryOutboxForDispatcher(ctx, first.IdempotencyKey, "legacy-gateway-dispatcher", now); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(delivery.HTTPHandler(gateway, delivery.HTTPOptions{ControlPlaneToken: "control-token"}))
	defer server.Close()
	projector := delivery.HTTPProjector{URL: server.URL, ControlPlaneToken: "control-token", Client: &http.Client{Timeout: time.Second}}
	if err := projector.ProjectWorkflowInbox(ctx, "owner/repo", nil); err != nil {
		t.Fatalf("serialized Inbox projection = %v", err)
	}
}

func TestHTTPProjectorAcceptsSameInboxClaimContention(t *testing.T) {
	ctx := context.Background()
	db, _ := newAcceptedClaim(t, ctx)
	defer db.Close()
	now := time.Date(2026, 8, 7, 4, 0, 0, 0, time.UTC)
	gateway := delivery.Gateway{Store: db, Remote: &fakeRemote{}, Now: func() time.Time { return now }}
	queued, err := gateway.Submit(ctx, store.DeliveryRequest{Operation: store.DeliveryProjectInbox, Repository: "owner/repo"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureGatewayDispatcher(ctx, "legacy-gateway-dispatcher", now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimDeliveryOutboxForDispatcher(ctx, queued.IdempotencyKey, "legacy-gateway-dispatcher", now); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(delivery.HTTPHandler(gateway, delivery.HTTPOptions{ControlPlaneToken: "control-token"}))
	defer server.Close()
	projector := delivery.HTTPProjector{URL: server.URL, ControlPlaneToken: "control-token", Client: &http.Client{Timeout: time.Second}}
	if err := projector.ProjectWorkflowInbox(ctx, "owner/repo", nil); err != nil {
		t.Fatalf("same Inbox contention = %v", err)
	}
}

func TestGatewayRejectsClaimedInboxWhenCurrentPlanCompletes(t *testing.T) {
	ctx := context.Background()
	db, claim := newAcceptedClaim(t, ctx)
	defer db.Close()
	now := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)
	remote := &fakeRemote{}
	gateway := delivery.Gateway{Store: db, Remote: remote, Now: func() time.Time { return now }}
	outbox, err := gateway.Submit(ctx, store.DeliveryRequest{Operation: store.DeliveryProjectInbox, Repository: "owner/repo"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkTicketDelivered(ctx, claim.VersionID, claim.TicketID); err != nil {
		t.Fatal(err)
	}
	if err := gateway.Dispatch(ctx, outbox.IdempotencyKey); !errors.Is(err, store.ErrNoActiveDeliveryPlan) {
		t.Fatalf("completed-plan dispatch error = %v, want no active delivery plan", err)
	}
	if remote.observeCalls != 0 || remote.applyCalls != 0 {
		t.Fatalf("inactive Inbox reached remote: observe=%d apply=%d", remote.observeCalls, remote.applyCalls)
	}
}

func TestRejectedInactiveInboxKeyDoesNotPoisonReplacementPlan(t *testing.T) {
	ctx := context.Background()
	db, claim := newAcceptedClaim(t, ctx)
	defer db.Close()
	now := time.Now().UTC()
	remote := &fakeRemote{}
	gateway := delivery.Gateway{Store: db, Remote: remote, Now: func() time.Time { return now }}
	first, err := gateway.Submit(ctx, store.DeliveryRequest{Operation: store.DeliveryProjectInbox, Repository: "owner/repo"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Request.InboxPlanVersionID != claim.VersionID {
		t.Fatalf("first Inbox plan version = %q, want %q", first.Request.InboxPlanVersionID, claim.VersionID)
	}
	if err := db.MarkTicketDelivered(ctx, claim.VersionID, claim.TicketID); err != nil {
		t.Fatal(err)
	}
	if err := gateway.Dispatch(ctx, first.IdempotencyKey); !errors.Is(err, store.ErrNoActiveDeliveryPlan) {
		t.Fatalf("inactive first dispatch = %v", err)
	}
	snapshot := plan.Snapshot{
		Repository: "owner/repo",
		Root:       plan.Issue{ID: 200, Number: 20, Labels: []string{plan.PlanLabel}, UpdatedAt: "replacement"},
		Children:   []plan.Issue{{ID: 2, Number: 12, Title: "replacement", Labels: []string{plan.TicketLabel}, State: "open"}},
	}
	fingerprint, err := snapshot.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := db.BeginActivation(ctx, snapshot, fingerprint, "replacement")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkActive(ctx, replacement.ID); err != nil {
		t.Fatal(err)
	}
	now = time.Now().UTC()
	second, err := gateway.Submit(ctx, store.DeliveryRequest{Operation: store.DeliveryProjectInbox, Repository: "owner/repo"})
	if err != nil {
		t.Fatal(err)
	}
	if second.IdempotencyKey == first.IdempotencyKey || second.Request.InboxPlanVersionID != replacement.ID {
		t.Fatalf("replacement Inbox = %#v; first key=%q", second, first.IdempotencyKey)
	}
	if err := gateway.Dispatch(ctx, second.IdempotencyKey); err != nil {
		t.Fatalf("replacement Inbox dispatch = %v", err)
	}
	if remote.applyCalls != 1 {
		t.Fatalf("replacement Inbox apply calls = %d, want 1", remote.applyCalls)
	}
}

func TestGatewayClearsUncertainInboxReplayWhenCurrentPlanCompletes(t *testing.T) {
	ctx := context.Background()
	db, claim := newAcceptedClaim(t, ctx)
	defer db.Close()
	now := time.Now().UTC()
	outbox, err := db.EnqueueDelivery(ctx, store.DeliveryRequest{Operation: store.DeliveryProjectInbox, Repository: "owner/repo"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureGatewayDispatcher(ctx, "old-dispatcher", now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimDeliveryOutboxForDispatcher(ctx, outbox.IdempotencyKey, "old-dispatcher", now); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkTicketDelivered(ctx, claim.VersionID, claim.TicketID); err != nil {
		t.Fatal(err)
	}
	remote := &fakeRemote{}
	gateway := delivery.Gateway{Store: db, Remote: remote, DispatcherToken: "new-dispatcher", Now: func() time.Time { return now.Add(time.Hour) }}
	if err := gateway.Dispatch(ctx, outbox.IdempotencyKey); !errors.Is(err, store.ErrNoActiveDeliveryPlan) {
		t.Fatalf("inactive uncertain replay error = %v", err)
	}
	if err := gateway.DispatchPending(ctx, 8); err != nil {
		t.Fatal(err)
	}
	if remote.observeCalls != 1 || remote.applyCalls != 1 || len(remote.requests) != 1 || len(remote.requests[0].WorkflowQuestions) != 0 {
		t.Fatalf("durable inactive Inbox projection: observe=%d apply=%d requests=%#v", remote.observeCalls, remote.applyCalls, remote.requests)
	}
}

func TestGatewayCorrectsInboxWhenPlanCompletesDuringApply(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "workflow.db")
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	peer, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	snapshot := plan.Snapshot{Repository: "owner/repo", Root: plan.Issue{ID: 1, Number: 1, Labels: []string{plan.PlanLabel}}, Children: []plan.Issue{{ID: 2, Number: 2, Labels: []string{plan.TicketLabel}, State: "open"}}}
	fingerprint, err := snapshot.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	version, err := db.BeginActivation(ctx, snapshot, fingerprint, "race")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkActive(ctx, version.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	advanced := false
	remote := &advancingRemote{fakeRemote: fakeRemote{}, advance: func() {
		if !advanced {
			advanced = true
			if err := peer.MarkTicketDelivered(ctx, version.ID, 2); err != nil {
				t.Error(err)
			}
		}
	}}
	gateway := delivery.Gateway{Store: db, Remote: remote, Now: func() time.Time { return now }}
	queued, err := gateway.Submit(ctx, store.DeliveryRequest{Operation: store.DeliveryProjectInbox, Repository: "owner/repo"})
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Dispatch(ctx, queued.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if err := gateway.DispatchPending(ctx, 8); err != nil {
		t.Fatal(err)
	}
	if remote.applyCalls != 2 || len(remote.requests) < 2 || len(remote.requests[len(remote.requests)-1].WorkflowQuestions) != 0 {
		t.Fatalf("durable Inbox delivery = applies %d requests %#v", remote.applyCalls, remote.requests)
	}
}

func TestGatewayQueuesEmptyInboxWhenPlanCompletesDuringAppliedObservation(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "workflow.db")
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	peer, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	snapshot := plan.Snapshot{Repository: "owner/repo", Root: plan.Issue{ID: 1, Number: 1, Labels: []string{plan.PlanLabel}}, Children: []plan.Issue{{ID: 2, Number: 2, Labels: []string{plan.TicketLabel}, State: "open"}}}
	fingerprint, err := snapshot.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	version, err := db.BeginActivation(ctx, snapshot, fingerprint, "observation-race")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkActive(ctx, version.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	remote := &observingAdvanceRemote{fakeRemote: fakeRemote{observations: []delivery.Observation{{Applied: true}}}, advance: func() {
		if err := peer.MarkTicketDelivered(ctx, version.ID, 2); err != nil {
			t.Error(err)
		}
	}}
	gateway := delivery.Gateway{Store: db, Remote: remote, Now: func() time.Time { return now }}
	queued, err := gateway.Submit(ctx, store.DeliveryRequest{Operation: store.DeliveryProjectInbox, Repository: "owner/repo"})
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Dispatch(ctx, queued.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if err := gateway.DispatchPending(ctx, 8); err != nil {
		t.Fatal(err)
	}
	if remote.applyCalls != 1 || len(remote.requests) < 2 || len(remote.requests[len(remote.requests)-1].WorkflowQuestions) != 0 {
		t.Fatalf("durable observation correction = applies %d requests %#v", remote.applyCalls, remote.requests)
	}
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

type advancingRemote struct {
	fakeRemote
	advance func()
}

type observingAdvanceRemote struct {
	fakeRemote
	advance func()
}

func (r *advancingRemote) Apply(ctx context.Context, request store.DeliveryRequest) (delivery.Observation, error) {
	r.advance()
	return r.fakeRemote.Apply(ctx, request)
}

func (r *observingAdvanceRemote) Observe(ctx context.Context, request store.DeliveryRequest) (delivery.Observation, error) {
	advance := r.advance
	r.advance = func() {}
	advance()
	return r.fakeRemote.Observe(ctx, request)
}

func (r *blockingRemote) Observe(context.Context, store.DeliveryRequest) (delivery.Observation, error) {
	return delivery.Observation{RemoteHead: "base", RemoteExists: true}, nil
}

func (r *blockingRemote) Apply(context.Context, store.DeliveryRequest) (delivery.Observation, error) {
	close(r.entered)
	<-r.release
	return delivery.Observation{Applied: true, RemoteHead: "accepted", RemoteExists: true}, nil
}

type credentialBarrierRemote struct {
	fakeRemote
	credentialEntered chan struct{}
	releaseCredential chan struct{}
}

func (r *credentialBarrierRemote) CredentialAvailable(context.Context) error {
	close(r.credentialEntered)
	<-r.releaseCredential
	return nil
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

func TestMissingCredentialPausesBeforeAnyRemoteCall(t *testing.T) {
	ctx := context.Background()
	db, claim := newAcceptedClaim(t, ctx)
	defer db.Close()
	remote := &fakeRemote{credentialErr: delivery.ErrGatewayCredentialRejected}
	gateway := delivery.Gateway{Store: db, Remote: remote, Now: func() time.Time {
		return time.Date(2026, 7, 31, 0, 30, 0, 0, time.UTC)
	}}
	queued, err := gateway.Submit(ctx, store.DeliveryRequest{
		Operation: store.DeliveryPushCandidate, RunID: claim.RunID, LeaseToken: claim.LeaseToken,
		LeaseGeneration: claim.LeaseGeneration, CommitSHA: "accepted", ExpectedRemoteHead: "base",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Dispatch(ctx, queued.IdempotencyKey); !errors.Is(err, delivery.ErrGatewayWritesPaused) {
		t.Fatalf("dispatch error = %v", err)
	}
	if remote.observeCalls != 0 || remote.applyCalls != 0 {
		t.Fatal("missing Gateway Credential reached the remote")
	}
	if _, err := db.WorkflowInboxItem(ctx, store.GatewayCredentialInboxKey); err != nil {
		t.Fatal(err)
	}
	outbox, err := db.DeliveryOutbox(ctx, queued.IdempotencyKey)
	if err != nil || outbox.State != store.OutboxPending {
		t.Fatalf("outbox after unavailable credential = %#v, %v", outbox, err)
	}
}

func TestGatewayRequeuesTransientCredentialSourceFailure(t *testing.T) {
	ctx := context.Background()
	db, claim := newAcceptedClaim(t, ctx)
	defer db.Close()
	now := time.Date(2026, 7, 31, 0, 30, 0, 0, time.UTC)
	remote := &fakeRemote{credentialErr: errors.New("credential manager temporarily unavailable")}
	gateway := delivery.Gateway{Store: db, Remote: remote, Now: func() time.Time { return now }}
	queued, err := gateway.Submit(ctx, candidatePush(claim, "base", false))
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Dispatch(ctx, queued.IdempotencyKey); err == nil {
		t.Fatal("transient credential failure returned nil")
	}
	paused, _, err := db.GatewayWritesPaused(ctx)
	if err != nil || paused {
		t.Fatalf("Gateway pause = %t, %v", paused, err)
	}
	outbox, err := db.DeliveryOutbox(ctx, queued.IdempotencyKey)
	if err != nil || outbox.State != store.OutboxPending || outbox.NextAttemptAt == nil {
		t.Fatalf("transient credential outbox = %#v, %v", outbox, err)
	}
}

func TestGatewayDefersRateLimitedWriteUntilGitHubRetryTime(t *testing.T) {
	ctx := context.Background()
	db, claim := newAcceptedClaim(t, ctx)
	defer db.Close()
	now := time.Date(2026, 7, 31, 0, 30, 0, 0, time.UTC)
	retryAfter := now.Add(7 * time.Minute)
	remote := &fakeRemote{observations: []delivery.Observation{{RemoteHead: "base", RemoteExists: true}}, applyErr: &githubapi.APIError{Method: "POST", Path: "/repos/owner/repo", StatusCode: 403, RetryAt: retryAfter}}
	gateway := delivery.Gateway{Store: db, Remote: remote, Now: func() time.Time { return now }}
	queued, err := gateway.Submit(ctx, candidatePush(claim, "base", false))
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Dispatch(ctx, queued.IdempotencyKey); err == nil {
		t.Fatal("rate-limited delivery returned nil")
	}
	paused, _, err := db.GatewayWritesPaused(ctx)
	if err != nil || paused {
		t.Fatalf("Gateway pause = %t, %v", paused, err)
	}
	outbox, err := db.DeliveryOutbox(ctx, queued.IdempotencyKey)
	if err != nil || outbox.State != store.OutboxPending || outbox.NextAttemptAt == nil || !outbox.NextAttemptAt.Equal(retryAfter) {
		t.Fatalf("rate-limited outbox = %#v, %v", outbox, err)
	}
}

func TestCredentialBindingFollowsDurableDispatchAdmission(t *testing.T) {
	ctx := context.Background()
	db, claim := newAcceptedClaim(t, ctx)
	defer db.Close()
	now := time.Date(2026, 7, 31, 0, 30, 0, 0, time.UTC)
	remote := &credentialBarrierRemote{
		fakeRemote:        fakeRemote{observations: []delivery.Observation{{RemoteHead: "base", RemoteExists: true}}},
		credentialEntered: make(chan struct{}),
		releaseCredential: make(chan struct{}),
	}
	gateway := delivery.Gateway{Store: db, Remote: remote, Now: func() time.Time { return now }}
	queued, err := gateway.Submit(ctx, candidatePush(claim, "base", false))
	if err != nil {
		t.Fatal(err)
	}
	dispatched := make(chan error, 1)
	go func() { dispatched <- gateway.Dispatch(ctx, queued.IdempotencyKey) }()
	select {
	case <-remote.credentialEntered:
	case <-time.After(time.Second):
		t.Fatal("dispatch did not reach credential binding")
	}
	quiesced := make(chan error, 1)
	go func() { quiesced <- db.WaitForGatewayWritesQuiesced(ctx) }()
	select {
	case err := <-quiesced:
		t.Fatalf("rotation could bypass credential-binding dispatch: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(remote.releaseCredential)
	if err := <-dispatched; err != nil {
		t.Fatal(err)
	}
	if err := <-quiesced; err != nil {
		t.Fatal(err)
	}
}

func TestGatewayRequeuesControlPlaneClaimAfterCancellationBeforeRenewal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	db, _ := newAcceptedClaim(t, ctx)
	defer db.Close()
	now := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	calls := 0
	gateway := delivery.Gateway{Store: db, Remote: &fakeRemote{}, Now: func() time.Time {
		calls++
		if calls == 4 {
			cancel()
		}
		return now
	}}
	queued, err := gateway.Submit(ctx, store.DeliveryRequest{Operation: store.DeliveryProjectInbox, Repository: "owner/repo"})
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Dispatch(ctx, queued.IdempotencyKey); err == nil {
		t.Fatal("cancelled control-plane dispatch returned nil error")
	}
	outbox, err := db.DeliveryOutbox(context.Background(), queued.IdempotencyKey)
	if err != nil || outbox.State != store.OutboxPending || outbox.ClaimToken != "" {
		t.Fatalf("cancelled control-plane outbox = %#v, %v", outbox, err)
	}
	now = now.Add(2 * time.Second)
	if err := gateway.Dispatch(context.Background(), queued.IdempotencyKey); err != nil {
		t.Fatalf("recovered control-plane dispatch = %v", err)
	}
}

func TestGatewayRequeuesUncertainControlPlaneClaimAfterLostLease(t *testing.T) {
	ctx := context.Background()
	db, _ := newAcceptedClaim(t, ctx)
	defer db.Close()
	now := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	remote := &advancingRemote{
		fakeRemote: fakeRemote{observations: []delivery.Observation{{}, {Applied: true, RemoteHead: "accepted", RemoteExists: true}}},
		advance:    func() { now = now.Add(2 * time.Hour) },
	}
	gateway := delivery.Gateway{Store: db, Remote: remote, Now: func() time.Time { return now }}
	queued, err := gateway.Submit(ctx, store.DeliveryRequest{Operation: store.DeliveryProjectInbox, Repository: "owner/repo"})
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Dispatch(ctx, queued.IdempotencyKey); err == nil {
		t.Fatal("lost control-plane lease returned nil error")
	}
	outbox, err := db.DeliveryOutbox(ctx, queued.IdempotencyKey)
	if err != nil || outbox.State != store.OutboxPending || !outbox.Uncertain || outbox.ClaimToken != "" {
		t.Fatalf("lost-lease control-plane outbox = %#v, %v", outbox, err)
	}
	now = now.Add(2 * time.Second)
	if err := gateway.Dispatch(ctx, queued.IdempotencyKey); err != nil {
		t.Fatalf("recovered lost-lease control-plane dispatch = %v", err)
	}
	outbox, err = db.DeliveryOutbox(ctx, queued.IdempotencyKey)
	if err != nil || outbox.State != store.OutboxSucceeded {
		t.Fatalf("reconciled lost-lease control-plane outbox = %#v, %v", outbox, err)
	}
}

func TestGatewayRequeuesClaimWhenCompletionFinalizationFails(t *testing.T) {
	ctx := context.Background()
	db, claim := newAcceptedClaim(t, ctx)
	defer db.Close()
	now := time.Date(2026, 7, 31, 0, 30, 0, 0, time.UTC)
	gateway := delivery.Gateway{Store: completionFailingStore{Store: db, err: errors.New("completion store temporarily unavailable")}, Remote: &fakeRemote{observations: []delivery.Observation{{RemoteHead: "base"}}}, Now: func() time.Time { return now }}
	queued, err := gateway.Submit(ctx, candidatePush(claim, "base", false))
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Dispatch(ctx, queued.IdempotencyKey); err == nil {
		t.Fatal("dispatch with an unfinalizable delivery returned nil error")
	}
	outbox, err := db.DeliveryOutbox(ctx, queued.IdempotencyKey)
	if err != nil || outbox.State != store.OutboxPending || !outbox.Uncertain || outbox.ClaimToken != "" {
		t.Fatalf("completion failure outbox = %#v, %v", outbox, err)
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
	gateway := delivery.Gateway{Store: db, Remote: remote, Now: func() time.Time { return time.Date(2026, 7, 31, 0, 30, 0, 0, time.UTC) }}
	queued, err := gateway.Submit(ctx, candidatePush(claim, "base", false))
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

func TestGatewayDispatchPendingPublishesAcceptedCandidateInOrder(t *testing.T) {
	ctx := context.Background()
	db, claim := newPublishedCandidate(t, ctx)
	defer db.Close()
	remote := &fakeRemote{observations: []delivery.Observation{{RemoteHead: "base", RemoteExists: true}, {RemoteHead: "accepted", RemoteExists: true}}}
	gateway := delivery.Gateway{Store: db, Remote: remote, Now: func() time.Time { return time.Date(2026, 7, 31, 0, 30, 0, 0, time.UTC) }}
	push, err := gateway.Submit(ctx, candidatePush(claim, "base", false))
	if err != nil {
		t.Fatal(err)
	}
	pr, err := gateway.Submit(ctx, candidatePR(claim))
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.DispatchPending(ctx, 8); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{push.IdempotencyKey, pr.IdempotencyKey} {
		outbox, err := db.DeliveryOutbox(ctx, key)
		if err != nil || outbox.State != store.OutboxSucceeded {
			t.Fatalf("outbox %q = %#v, %v", key, outbox, err)
		}
	}
	if remote.applyCalls != 2 {
		t.Fatalf("apply calls = %d", remote.applyCalls)
	}
}

func TestGatewayDispatchPendingReportsFailingDeliveryKey(t *testing.T) {
	ctx := context.Background()
	db, _ := newAcceptedClaim(t, ctx)
	defer db.Close()
	now := time.Date(2026, 8, 7, 5, 0, 0, 0, time.UTC)
	gateway := delivery.Gateway{Store: db, Remote: &fakeRemote{applyErr: errors.New("remote unavailable")}, Now: func() time.Time { return now }}
	queued, err := gateway.Submit(ctx, store.DeliveryRequest{Operation: store.DeliveryProjectInbox, Repository: "owner/repo"})
	if err != nil {
		t.Fatal(err)
	}
	err = gateway.DispatchPending(ctx, 1)
	if err == nil || !strings.Contains(err.Error(), queued.IdempotencyKey) {
		t.Fatalf("dispatch error = %v, want delivery key %q", err, queued.IdempotencyKey)
	}
}

func TestGatewayDispatchReportsTerminalUncertainInboxRecoveryKey(t *testing.T) {
	ctx := context.Background()
	db, _ := newAcceptedClaim(t, ctx)
	defer db.Close()
	now := time.Date(2026, 8, 7, 5, 30, 0, 0, time.UTC)
	queued, err := db.QueueWorkflowInboxProjection(ctx, "owner/repo", now)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		claim, claimErr := db.ClaimDeliveryOutbox(ctx, queued.IdempotencyKey, now)
		if claimErr != nil {
			t.Fatal(claimErr)
		}
		if err := db.RequeueDeliveryOutboxClaim(ctx, queued.IdempotencyKey, claim.ClaimToken, "remote outcome unknown", true, now); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Minute)
	}
	gateway := delivery.Gateway{Store: db, Remote: &fakeRemote{applyErr: errors.New("remote unavailable")}, Now: func() time.Time { return now }}
	err = gateway.Dispatch(ctx, queued.IdempotencyKey)
	if !errors.Is(err, store.ErrDeliveryRejected) || !strings.Contains(err.Error(), queued.IdempotencyKey) || !strings.Contains(err.Error(), "workflow recover-inbox-delivery") {
		t.Fatalf("terminal uncertain Inbox dispatch = %v", err)
	}
}

func TestGatewayPreservesUncertainInboxFenceWhenReconciliationIsRejected(t *testing.T) {
	ctx := context.Background()
	db, _ := newAcceptedClaim(t, ctx)
	defer db.Close()
	now := time.Date(2026, 8, 7, 5, 45, 0, 0, time.UTC)
	queued, err := db.QueueWorkflowInboxProjection(ctx, "owner/repo", now)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := db.ClaimDeliveryOutbox(ctx, queued.IdempotencyKey, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RequeueDeliveryOutboxClaim(ctx, queued.IdempotencyKey, claim.ClaimToken, "remote outcome unknown", true, now); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	gateway := delivery.Gateway{Store: db, Remote: &fakeRemote{observeErrs: []error{fmt.Errorf("%w: owner guard changed", store.ErrDeliveryRejected)}}, Now: func() time.Time { return now }}
	err = gateway.Dispatch(ctx, queued.IdempotencyKey)
	if !errors.Is(err, store.ErrDeliveryRejected) || !strings.Contains(err.Error(), "workflow recover-inbox-delivery") {
		t.Fatalf("reconciliation rejection = %v", err)
	}
	outbox, err := db.DeliveryOutbox(ctx, queued.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if outbox.State != store.OutboxRejected || !outbox.Uncertain || !strings.Contains(outbox.LastError, queued.IdempotencyKey) {
		t.Fatalf("rejected uncertain outbox = %#v", outbox)
	}
}

func TestGatewayPersistsUncertaintyAndAcceptsAppliedObservationBeforePreconditions(t *testing.T) {
	ctx := context.Background()
	db, claim := newAcceptedClaim(t, ctx)
	defer db.Close()
	now := time.Date(2026, 7, 31, 0, 30, 0, 0, time.UTC)
	remote := &fakeRemote{
		applyErr: errors.New("request timed out"),
		observations: []delivery.Observation{
			{RemoteHead: "base"},
			{RemoteHead: "base"},
			{Applied: true, RemoteHead: "accepted"},
		},
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

func TestGatewayBoundsUncertainReconciliation(t *testing.T) {
	ctx := context.Background()
	db, claim := newAcceptedClaim(t, ctx)
	defer db.Close()
	now := time.Date(2026, 7, 31, 0, 30, 0, 0, time.UTC)
	remote := &fakeRemote{
		applyErr:     errors.New("request timed out"),
		observations: []delivery.Observation{{RemoteHead: "base"}},
		observeErrs:  []error{nil, errors.New("ambiguous write"), errors.New("reconciliation failed"), errors.New("reconciliation failed")},
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
	for range 2 {
		now = now.Add(2 * time.Second)
		if err := gateway.Dispatch(ctx, queued.IdempotencyKey); err == nil {
			t.Fatal("failed reconciliation returned nil error")
		}
	}
	outbox, err := db.DeliveryOutbox(ctx, queued.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if outbox.State != store.OutboxRejected || outbox.Attempts != 3 || remote.applyCalls != 1 {
		t.Fatalf("bounded uncertain reconciliation = %#v, applies=%d", outbox, remote.applyCalls)
	}
	projection, err := db.PlanProjectionAt(ctx, claim.VersionID, now)
	if err != nil {
		t.Fatal(err)
	}
	if projection.Tickets[0].State != "Needs Attention" {
		t.Fatalf("projection after exhausted reconciliation = %#v", projection)
	}
}

func TestGatewayDerivesRepositoryFromLeasedTicket(t *testing.T) {
	ctx := context.Background()
	db, claim := newAcceptedClaim(t, ctx)
	defer db.Close()
	queued, err := db.EnqueueDelivery(ctx, store.DeliveryRequest{
		Operation: store.DeliveryPushCandidate, RunID: claim.RunID, LeaseToken: claim.LeaseToken,
		LeaseGeneration: claim.LeaseGeneration, Branch: "ticket-1", CommitSHA: "accepted", ExpectedRemoteHead: "base",
	}, time.Date(2026, 7, 31, 0, 10, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if queued.Request.Repository != "owner/repo" {
		t.Fatalf("repository = %q, want ticket-owned repository", queued.Request.Repository)
	}
}

func TestRejectedCredentialPausesAllGatewayWritesAndCreatesOneInboxItem(t *testing.T) {
	ctx := context.Background()
	db, claim := newAcceptedClaim(t, ctx)
	defer db.Close()
	remote := &fakeRemote{
		applyErr:     &githubapi.APIError{Method: "POST", Path: "/repos/owner/repo", StatusCode: 401, Body: "Bad credentials"},
		observations: []delivery.Observation{{RemoteHead: "base"}},
	}
	gateway := delivery.Gateway{Store: db, Remote: remote, Now: func() time.Time {
		return time.Date(2026, 7, 31, 0, 30, 0, 0, time.UTC)
	}}
	queued, err := gateway.Submit(ctx, store.DeliveryRequest{
		Operation: store.DeliveryPushCandidate, RunID: claim.RunID, LeaseToken: claim.LeaseToken,
		LeaseGeneration: claim.LeaseGeneration, CommitSHA: "accepted", ExpectedRemoteHead: "base",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Dispatch(ctx, queued.IdempotencyKey); !errors.Is(err, delivery.ErrGatewayWritesPaused) {
		t.Fatalf("dispatch error = %v", err)
	}
	item, err := db.WorkflowInboxItem(ctx, store.GatewayCredentialInboxKey)
	if err != nil || item.State != "open" {
		t.Fatalf("inbox item = %#v, %v", item, err)
	}
	applyCalls := remote.applyCalls
	if err := gateway.Dispatch(ctx, queued.IdempotencyKey); !errors.Is(err, delivery.ErrGatewayWritesPaused) {
		t.Fatalf("paused dispatch error = %v", err)
	}
	if remote.applyCalls != applyCalls {
		t.Fatal("paused Gateway performed another remote write")
	}
	outbox, err := db.DeliveryOutbox(ctx, queued.IdempotencyKey)
	if err != nil || outbox.State != store.OutboxPending {
		t.Fatalf("preserved outbox = %#v, %v", outbox, err)
	}
}

func TestGatewayRejectsZombieCommandAfterLeaseReplacement(t *testing.T) {
	ctx := context.Background()
	db, claim := newAcceptedClaim(t, ctx)
	defer db.Close()
	remote := &fakeRemote{observations: []delivery.Observation{{RemoteHead: "base"}}}
	if err := db.RecordRunFailure(ctx, store.RunFailure{RunID: claim.RunID, LeaseToken: claim.LeaseToken, DiagnosticsPath: "diagnostics", Error: "worker replaced", Now: time.Now().UTC()}); err != nil {
		if !errors.Is(err, store.ErrInvalidClaim) {
			t.Fatal(err)
		}
	}
	if remote.applyCalls != 0 {
		t.Fatal("zombie command reached remote")
	}
}

func TestGatewayAllowsFirstCandidatePushToExpectAbsentBranch(t *testing.T) {
	ctx := context.Background()
	db, claim := newCandidateClaimWithPublication(t, ctx, store.CandidatePublication{Repository: "owner/repo", Branch: "ticket-1", ExpectRemoteAbsent: true, Title: "ticket", Body: "evidence"})
	defer db.Close()
	remote := &fakeRemote{observations: []delivery.Observation{{}}}
	gateway := delivery.Gateway{Store: db, Remote: remote, Now: func() time.Time { return time.Date(2026, 7, 31, 0, 30, 0, 0, time.UTC) }}
	queued, err := gateway.Submit(ctx, candidatePush(claim, "", true))
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

func TestGatewayRejectsAgentPhasePublicationBeforeDeliveryController(t *testing.T) {
	ctx := context.Background()
	db, claim := newAgentCandidate(t, ctx)
	defer db.Close()
	remote := &fakeRemote{observations: []delivery.Observation{{RemoteHead: "base"}}}
	gateway := delivery.Gateway{Store: db, Remote: remote, Now: func() time.Time { return time.Date(2026, 7, 31, 0, 1, 30, 0, time.UTC) }}
	if _, err := gateway.Submit(ctx, candidatePush(claim, "base", false)); !errors.Is(err, store.ErrDeliveryRejected) {
		t.Fatalf("Agent publication error = %v, want delivery rejection", err)
	}
	if remote.applyCalls != 0 {
		t.Fatalf("Agent publication reached remote: applies=%d", remote.applyCalls)
	}
}

func TestGatewayAllowsDeliveryControllerCommandFromAcceptedCandidate(t *testing.T) {
	ctx := context.Background()
	db, claim := newPublishedCandidate(t, ctx)
	defer db.Close()
	queued, err := db.EnqueueDelivery(ctx, store.DeliveryRequest{
		Operation: store.DeliveryUpsertPR, RunID: claim.RunID, LeaseToken: claim.LeaseToken, LeaseGeneration: claim.LeaseGeneration,
		Repository: "owner/repo", Branch: "ticket-1", CommitSHA: "accepted", ExpectedRemoteHead: "accepted", Title: "mutated title", Body: "mutable body",
	}, time.Date(2026, 7, 31, 0, 2, 0, 0, time.UTC))
	if err != nil || queued.Request.Operation != store.DeliveryUpsertPR {
		t.Fatalf("delivery controller command = %#v, err = %v", queued, err)
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
	gateway := delivery.Gateway{Store: db, Remote: remote, Now: func() time.Time { return time.Date(2026, 7, 31, 0, 30, 0, 0, time.UTC) }}
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
	if len(audits) < 3 || audits[len(audits)-2].Decision != "rejected" || audits[len(audits)-1].Decision != "needs_attention" {
		t.Fatalf("audits = %#v", audits)
	}
}

func TestLeaseTakeoverCannotCommitAcrossInflightExternalWrite(t *testing.T) {
	ctx := context.Background()
	db, claim := newAcceptedClaim(t, ctx)
	defer db.Close()
	remote := &blockingRemote{entered: make(chan struct{}), release: make(chan struct{})}
	gateway := delivery.Gateway{Store: db, Remote: remote, Now: func() time.Time { return time.Date(2026, 7, 31, 0, 30, 0, 0, time.UTC) }}
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
		takeover <- db.MarkTicketDelivered(ctx, claim.VersionID, claim.TicketID)
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
		return time.Date(2026, 7, 31, 0, 59, 59, 950000000, time.UTC)
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
	outbox, err := db.DeliveryOutbox(ctx, queued.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if !outbox.Uncertain || outbox.State != store.OutboxPending {
		t.Fatalf("expired post-write outbox = %#v", outbox)
	}
	if !remote.deadlineSeen || time.Since(started) > time.Second {
		t.Fatalf("external write was not bounded by lease deadline; deadline=%t elapsed=%s", remote.deadlineSeen, time.Since(started))
	}
}

func TestGatewayRejectionPausesAcceptedCandidateDelivery(t *testing.T) {
	ctx := context.Background()
	db, claim := newAcceptedClaim(t, ctx)
	defer db.Close()
	remote := &fakeRemote{observations: []delivery.Observation{{RemoteHead: "unexpected", RemoteExists: true}}}
	now := time.Date(2026, 7, 31, 0, 30, 0, 0, time.UTC)
	gateway := delivery.Gateway{Store: db, Remote: remote, Now: func() time.Time { return now }}
	queued, err := gateway.Submit(ctx, candidatePush(claim, "base", false))
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Dispatch(ctx, queued.IdempotencyKey); !errors.Is(err, store.ErrDeliveryRejected) {
		t.Fatalf("push rejection error = %v, want delivery rejection", err)
	}
	projection, err := db.PlanProjectionAt(ctx, claim.VersionID, now)
	if err != nil {
		t.Fatal(err)
	}
	if projection.Tickets[0].State != "Needs Attention" {
		t.Fatalf("ticket state after rejected publication = %q", projection.Tickets[0].State)
	}
}

func TestGatewayTreatsPostDeadlineSuccessAsUncertain(t *testing.T) {
	ctx := context.Background()
	db, claim := newAcceptedClaim(t, ctx)
	defer db.Close()
	remote := &blockingRemote{entered: make(chan struct{}), release: make(chan struct{})}
	gateway := delivery.Gateway{Store: db, Remote: remote, Now: func() time.Time {
		return time.Date(2026, 7, 31, 0, 59, 59, 950000000, time.UTC)
	}}
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
	time.Sleep(75 * time.Millisecond)
	close(remote.release)
	if err := <-dispatched; !errors.Is(err, store.ErrDeliveryUncertain) {
		t.Fatalf("post-deadline success error = %v, want uncertain outcome", err)
	}
	outbox, err := db.DeliveryOutbox(ctx, queued.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if outbox.State != store.OutboxPending || !outbox.Uncertain {
		t.Fatalf("post-deadline success outbox = %#v", outbox)
	}
}

func TestGatewayRejectsDeliveryWhenValidationConsumesLease(t *testing.T) {
	ctx := context.Background()
	db, claim := newAcceptedClaim(t, ctx)
	defer db.Close()
	clock := []time.Time{
		time.Date(2026, 7, 31, 0, 59, 0, 0, time.UTC),
		time.Date(2026, 7, 31, 0, 59, 0, 0, time.UTC),
		time.Date(2026, 7, 31, 0, 59, 0, 0, time.UTC),
		time.Date(2026, 7, 31, 1, 3, 0, 0, time.UTC),
	}
	remote := &fakeRemote{observations: []delivery.Observation{{RemoteHead: "base"}}}
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
	if err := gateway.Dispatch(ctx, queued.IdempotencyKey); !errors.Is(err, store.ErrDeliveryRejected) {
		t.Fatalf("expired delivery error = %v, want delivery rejection", err)
	}
	if remote.observeCalls != 0 || remote.applyCalls != 0 {
		t.Fatalf("expired delivery reached remote: observes=%d applies=%d", remote.observeCalls, remote.applyCalls)
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
	if reclaimed.Attempts != 2 || reclaimed.State != store.OutboxProcessing || !reclaimed.ReconcileOnly || !reclaimed.Uncertain {
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
	}, time.Date(2026, 7, 31, 0, 3, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimDeliveryOutbox(ctx, queued.IdempotencyKey, time.Date(2026, 7, 31, 0, 3, 0, 0, time.UTC)); err != nil {
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
	remote := &fakeRemote{observations: []delivery.Observation{{RemoteHead: "base", RemoteExists: true}}}
	gateway := delivery.Gateway{Store: db, Remote: remote, Now: func() time.Time { return time.Date(2026, 7, 31, 0, 30, 0, 0, time.UTC) }}
	push, err := gateway.Submit(ctx, candidatePush(claim, "base", false))
	if err != nil {
		t.Fatal(err)
	}
	pr, err := gateway.Submit(ctx, candidatePR(claim))
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Dispatch(ctx, push.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkTicketDelivered(ctx, claim.VersionID, claim.TicketID); err != nil {
		t.Fatal(err)
	}
	applyCalls := remote.applyCalls
	for _, key := range []string{pr.IdempotencyKey} {
		if err := gateway.Dispatch(ctx, key); err == nil || !errors.Is(err, store.ErrDeliveryRejected) {
			t.Fatalf("zombie dispatch %q error = %v", key, err)
		}
	}
	if remote.applyCalls != applyCalls {
		t.Fatalf("zombie PR/reply reached remote: before=%d after=%d", applyCalls, remote.applyCalls)
	}
}

func newAcceptedClaim(t *testing.T, ctx context.Context) (*store.Store, store.TicketClaim) {
	return newCandidateClaim(t, ctx)
}

func newPublishedCandidate(t *testing.T, ctx context.Context) (*store.Store, store.TicketClaim) {
	return newCandidateClaim(t, ctx)
}

func newAgentCandidate(t *testing.T, ctx context.Context) (*store.Store, store.TicketClaim) {
	t.Helper()
	db, claim := newAgentClaim(t, ctx)
	if err := db.AcceptCandidate(ctx, store.CandidateRevision{RunID: claim.RunID, LeaseToken: claim.LeaseToken, CodexSessionID: "codex", CommitSHA: "accepted", StructuredOutput: []byte(`{"summary":"candidate","checks":[{"command":"go test","outcome":"passed"}]}`), Now: time.Date(2026, 7, 31, 0, 1, 0, 0, time.UTC), Publication: store.CandidatePublication{Repository: "owner/repo", Branch: "ticket-1", ExpectedRemoteHead: "base", Title: "ticket", Body: "evidence"}}); err != nil {
		t.Fatal(err)
	}
	return db, claim
}

func candidatePush(claim store.TicketClaim, expectedRemoteHead string, expectRemoteAbsent bool) store.DeliveryRequest {
	return store.DeliveryRequest{Operation: store.DeliveryPushCandidate, RunID: claim.RunID, LeaseToken: claim.LeaseToken, LeaseGeneration: claim.LeaseGeneration, Repository: "owner/repo", Branch: "ticket-1", CommitSHA: "accepted", ExpectedRemoteHead: expectedRemoteHead, ExpectRemoteAbsent: expectRemoteAbsent}
}

func candidatePR(claim store.TicketClaim) store.DeliveryRequest {
	return store.DeliveryRequest{Operation: store.DeliveryUpsertPR, RunID: claim.RunID, LeaseToken: claim.LeaseToken, LeaseGeneration: claim.LeaseGeneration, Repository: "owner/repo", Branch: "ticket-1", CommitSHA: "accepted", ExpectedRemoteHead: "accepted", Title: "ticket", Body: "evidence"}
}

func newCandidateClaim(t *testing.T, ctx context.Context) (*store.Store, store.TicketClaim) {
	return newCandidateClaimWithPublication(t, ctx, store.CandidatePublication{Repository: "owner/repo", Branch: "ticket-1", ExpectedRemoteHead: "base", Title: "ticket", Body: "evidence"})
}

func newCandidateClaimWithPublication(t *testing.T, ctx context.Context, publication store.CandidatePublication) (*store.Store, store.TicketClaim) {
	t.Helper()
	db, claim := newAgentClaim(t, ctx)
	delivery, err := db.AcceptCandidateForDelivery(ctx, store.CandidateRevision{RunID: claim.RunID, LeaseToken: claim.LeaseToken, CodexSessionID: "codex", CommitSHA: "accepted", StructuredOutput: []byte(`{"summary":"candidate","checks":[{"command":"go test","outcome":"passed"}]}`), Now: time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC), Publication: publication}, time.Hour)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	return db, delivery
}

func newAgentClaim(t *testing.T, ctx context.Context) (*store.Store, store.TicketClaim) {
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
	return db, claim
}
