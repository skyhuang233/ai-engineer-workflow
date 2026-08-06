package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/skyhuang233/workflow/internal/delivery"
	"github.com/skyhuang233/workflow/internal/plan"
	"github.com/skyhuang233/workflow/internal/store"
)

type countingInboxProjector struct {
	calls int
}

func (p *countingInboxProjector) ProjectWorkflowInbox(context.Context, string, []plan.WorkflowQuestion) error {
	p.calls++
	return nil
}

type activePlanInboxProjector struct {
	store *store.Store
	now   time.Time
	calls int
}

type recordingInboxProjector struct {
	delegate WorkflowInboxProjector
	err      error
}

func (p *recordingInboxProjector) ProjectWorkflowInbox(ctx context.Context, repository string, questions []plan.WorkflowQuestion) error {
	p.err = p.delegate.ProjectWorkflowInbox(ctx, repository, questions)
	return p.err
}

func (p *activePlanInboxProjector) ProjectWorkflowInbox(ctx context.Context, repository string, _ []plan.WorkflowQuestion) error {
	p.calls++
	_, err := p.store.EnqueueDelivery(ctx, store.DeliveryRequest{
		Operation:  store.DeliveryProjectInbox,
		Repository: repository,
	}, p.now)
	return err
}

type coldStartPlanReader struct {
	snapshot plan.Snapshot
}

func (r coldStartPlanReader) ReadPlan(context.Context, string, int64) (plan.Snapshot, error) {
	return r.snapshot, nil
}

type acceptingDeliveryRemote struct{}

func (acceptingDeliveryRemote) Observe(context.Context, store.DeliveryRequest) (delivery.Observation, error) {
	return delivery.Observation{}, nil
}

func (acceptingDeliveryRemote) Apply(context.Context, store.DeliveryRequest) (delivery.Observation, error) {
	return delivery.Observation{Applied: true}, nil
}

type coldStartProjector struct {
	store    *store.Store
	delegate delivery.HTTPProjector
	events   []string
}

func (p *coldStartProjector) ProjectPlan(ctx context.Context, repository string, rootNumber int64, projection plan.Projection, label string) error {
	active, err := p.store.HasActiveDeliveryPlan(ctx, repository)
	if err != nil {
		return err
	}
	p.events = append(p.events, "control: project Plan Root #2 (active="+fmt.Sprint(active)+")")
	return p.delegate.ProjectPlan(ctx, repository, rootNumber, projection, label)
}

func (p *coldStartProjector) ProjectWorkflowInbox(ctx context.Context, repository string, questions []plan.WorkflowQuestion) error {
	active, err := p.store.HasActiveDeliveryPlan(ctx, repository)
	if err != nil {
		return err
	}
	p.events = append(p.events, "poll: project Workflow Inbox (active="+fmt.Sprint(active)+")")
	return p.delegate.ProjectWorkflowInbox(ctx, repository, questions)
}

func TestColdStartActivatesApprovedPlanBeforeUnifiedInboxProjection(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := "owner/repo"
	now := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)
	controlToken := "control-token"
	gateway := delivery.Gateway{Store: db, Remote: acceptingDeliveryRemote{}, Now: func() time.Time { return now }}
	server := httptest.NewServer(delivery.HTTPHandler(gateway, delivery.HTTPOptions{ControlPlaneToken: controlToken}))
	defer server.Close()
	httpProjector := delivery.HTTPProjector{URL: server.URL, ControlPlaneToken: controlToken}
	if err := httpProjector.ProjectWorkflowInbox(ctx, repository, nil); err == nil {
		t.Fatal("Gateway admitted the repository before its Delivery Plan was active")
	} else {
		var gatewayErr *delivery.HTTPError
		if !errors.As(err, &gatewayErr) || gatewayErr.StatusCode != 409 || gatewayErr.Code != delivery.ErrorCodeNoActiveDeliveryPlan {
			t.Fatalf("cold-start Gateway rejection = %v", err)
		}
		t.Logf("before poll: Gateway rejected Workflow Inbox with HTTP %d %s", gatewayErr.StatusCode, gatewayErr.Code)
	}
	snapshot := plan.Snapshot{
		Repository: repository,
		Root:       plan.Issue{ID: 2, Number: 2, Body: "approved plan", Labels: []string{plan.PlanLabel}, UpdatedAt: "approved-root-2"},
		Children: []plan.Issue{
			{ID: 8, Number: 8, Title: "completed blocker", Labels: []string{plan.TicketLabel}, State: "closed", Delivered: true},
			{ID: 9, Number: 9, Title: "ready next ticket", Labels: []string{plan.TicketLabel}, State: "open"},
		},
		BlockedBy: map[int64][]plan.Issue{9: {{ID: 8, Number: 8, Title: "completed blocker", Labels: []string{plan.TicketLabel}, State: "closed", Delivered: true}}},
	}
	projector := &coldStartProjector{store: db, delegate: httpProjector}
	activator := plan.Activator{Reader: coldStartPlanReader{snapshot: snapshot}, Projector: projector, Store: db}
	bootstrap := func(ctx context.Context) error {
		_, err := activator.Activate(ctx, repository, 2)
		return err
	}
	_, err = (Poller{
		Store:          db,
		Client:         NewClient("http://example.invalid", "", nil).WithRepositoryOwner("owner"),
		InboxProjector: projector,
		Now:            func() time.Time { return now },
	}).PollWithBootstrap(ctx, repository, bootstrap, func(ctx context.Context, bootstrapped bool) error {
		if !bootstrapped {
			return bootstrap(ctx)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("cold-start poll = %v; sequence = %#v", err, projector.events)
	}
	wantEvents := []string{
		"control: project Plan Root #2 (active=false)",
		"control: project Plan Root #2 (active=true)",
		"poll: project Workflow Inbox (active=true)",
	}
	if !reflect.DeepEqual(projector.events, wantEvents) {
		t.Fatalf("cold-start sequence = %#v, want %#v", projector.events, wantEvents)
	}
	version, err := db.CurrentVersion(ctx, repository, 2)
	if err != nil {
		t.Fatal(err)
	}
	frontier, err := db.ReadyFrontier(ctx, version.ID, 1, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(frontier) != 1 || frontier[0].IssueID != 9 {
		t.Fatalf("ready frontier = %#v, want ticket #9", frontier)
	}
	for index, event := range projector.events {
		t.Logf("step %d: %s", index+1, event)
	}
	active, err := db.HasActiveDeliveryPlan(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("after poll: active Delivery Plan=%t; ready Executable Ticket=#%d", active, frontier[0].Number)
}

func TestPollRunsControlPassBeforeWorkflowInboxProjection(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)
	projector := &activePlanInboxProjector{store: db, now: now}
	_, err = (Poller{
		Store:          db,
		Client:         NewClient("http://example.invalid", "", nil).WithRepositoryOwner("owner"),
		InboxProjector: projector,
	}).PollWith(ctx, "owner/repo", func(context.Context) error {
		snapshot := plan.Snapshot{
			Repository: "owner/repo",
			Root:       plan.Issue{ID: 1, Number: 1, Labels: []string{plan.PlanLabel}},
			Children:   []plan.Issue{{ID: 2, Number: 2, Labels: []string{plan.TicketLabel}, State: "open"}},
		}
		fingerprint, err := snapshot.Fingerprint()
		if err != nil {
			return err
		}
		version, err := db.BeginActivation(ctx, snapshot, fingerprint, "cold-start")
		if err != nil {
			return err
		}
		return db.MarkActive(ctx, version.ID)
	})
	if err != nil {
		t.Fatal(err)
	}
	if projector.calls != 1 {
		t.Fatalf("inbox projector calls = %d, want 1", projector.calls)
	}
}

func TestPollBootstrapRecoversFailureBudgetWithoutActivePlan(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := "owner/repo"
	failedAt := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)
	now := failedAt.Add(time.Minute)
	controlToken := "control-token"
	requests := 0
	handler := delivery.HTTPHandler(delivery.Gateway{Store: db, Remote: acceptingDeliveryRemote{}, Now: func() time.Time { return failedAt }}, delivery.HTTPOptions{ControlPlaneToken: controlToken})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		handler.ServeHTTP(writer, request)
	}))
	defer server.Close()
	_, err = (Poller{
		Store:          db,
		Client:         NewClient("http://example.invalid", "", nil).WithRepositoryOwner("owner"),
		InboxProjector: delivery.HTTPProjector{URL: server.URL, ControlPlaneToken: controlToken},
		MaxFailures:    1,
		Now:            func() time.Time { return failedAt },
	}).Poll(ctx, repository)
	if !errors.Is(err, store.ErrNeedsAttention) {
		t.Fatalf("cold-start inbox conflict = %v, want needs attention", err)
	}
	if requests != 1 {
		t.Fatalf("cold-start Gateway requests = %d, want 1", requests)
	}
	cursor, err := db.GitHubPollCursor(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	if cursor.FailureKind != store.GitHubPollFailurePreActivationInboxConflict {
		t.Fatalf("failure kind = %q", cursor.FailureKind)
	}
	projector := &coldStartProjector{store: db, delegate: delivery.HTTPProjector{URL: server.URL, ControlPlaneToken: controlToken}}
	bootstrapRuns := 0
	controlRuns := 0
	_, err = (Poller{
		Store:          db,
		Client:         NewClient("http://example.invalid", "", nil).WithRepositoryOwner("owner"),
		InboxProjector: projector,
		MaxFailures:    1,
		Now:            func() time.Time { return now },
	}).PollWithBootstrap(ctx, repository, func(ctx context.Context) error {
		bootstrapRuns++
		activatePollerPlan(t, ctx, db, repository)
		return nil
	}, func(_ context.Context, bootstrapped bool) error {
		controlRuns++
		if !bootstrapped {
			t.Fatal("recovered poll did not report bootstrap completion")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if bootstrapRuns != 1 || controlRuns != 1 || len(projector.events) != 1 || projector.events[0] != "poll: project Workflow Inbox (active=true)" {
		t.Fatalf("bootstrap, control, projection = %d, %d, %#v", bootstrapRuns, controlRuns, projector.events)
	}
	cursor, err = db.GitHubPollCursor(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	if cursor.ConsecutiveFailures != 0 {
		t.Fatalf("consecutive failures = %d, want 0", cursor.ConsecutiveFailures)
	}
	t.Logf("recovery: classified cold-start failure budget reset to %d after bootstrap; %s", cursor.ConsecutiveFailures, projector.events[0])
}

func TestRecordFailureProjectsNeedsAttentionInboxForActivePlan(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := "owner/repo"
	activatePollerPlan(t, ctx, db, repository)
	now := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)
	controlToken := "control-token"
	requests := 0
	handler := delivery.HTTPHandler(delivery.Gateway{Store: db, Remote: acceptingDeliveryRemote{}, Now: func() time.Time { return now }}, delivery.HTTPOptions{ControlPlaneToken: controlToken})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		handler.ServeHTTP(writer, request)
	}))
	defer server.Close()

	_, err = (Poller{
		Store:          db,
		InboxProjector: delivery.HTTPProjector{URL: server.URL, ControlPlaneToken: controlToken},
		MaxFailures:    1,
		Now:            func() time.Time { return now },
	}).RecordFailure(ctx, repository, errors.New("GitHub read failed"))
	if !errors.Is(err, store.ErrNeedsAttention) {
		t.Fatalf("record failure = %v, want needs attention", err)
	}
	if requests != 1 {
		t.Fatalf("Needs Attention Inbox projections = %d, want 1", requests)
	}
}

func TestRecordFailureInvalidatesBootstrapProvenanceAfterSecondaryInboxError(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := "owner/repo"
	activatePollerPlan(t, ctx, db, repository)
	now := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)
	controlToken := "control-token"
	server := httptest.NewServer(delivery.HTTPHandler(delivery.Gateway{Store: db, Remote: acceptingDeliveryRemote{}, Now: func() time.Time { return now }}, delivery.HTTPOptions{ControlPlaneToken: controlToken}))
	defer server.Close()
	projector := &recordingInboxProjector{delegate: delivery.HTTPProjector{URL: server.URL, ControlPlaneToken: "wrong-token"}}
	preActivationConflict := &delivery.HTTPError{StatusCode: http.StatusConflict, Code: delivery.ErrorCodeNoActiveDeliveryPlan}

	_, err = (Poller{
		Store:          db,
		InboxProjector: projector,
		MaxFailures:    1,
	}).recordFailureWithKind(ctx, repository, now, store.GitHubPollFailurePreActivationInboxConflict, preActivationConflict)
	if !errors.Is(err, store.ErrNeedsAttention) {
		t.Fatalf("record failure = %v, want needs attention", err)
	}
	var gatewayErr *delivery.HTTPError
	if !errors.As(projector.err, &gatewayErr) || gatewayErr.StatusCode != http.StatusForbidden {
		t.Fatalf("secondary Inbox projection error = %#v, want HTTP 403", projector.err)
	}
	cursor, err := db.GitHubPollCursor(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	if cursor.FailureKind != store.GitHubPollFailureUnrecoverable {
		t.Fatalf("failure kind = %q, want unrecoverable", cursor.FailureKind)
	}
	recovered, err := db.RecoverGitHubPollAfterBootstrap(ctx, repository, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if recovered {
		t.Fatal("bootstrap recovered a cursor contaminated by a secondary error")
	}
}

func TestPollDoesNotBootstrapPastFailureBudgetWithUnclassifiedOrMixedFailures(t *testing.T) {
	for _, scenario := range []struct {
		name        string
		maxFailures int
		record      func(context.Context, *store.Store, string, time.Time) error
	}{
		{
			name:        "unclassified",
			maxFailures: 1,
			record: func(ctx context.Context, db *store.Store, repository string, now time.Time) error {
				return db.RecordGitHubPollFailure(ctx, repository, now)
			},
		},
		{
			name:        "mixed",
			maxFailures: 2,
			record: func(ctx context.Context, db *store.Store, repository string, now time.Time) error {
				if err := db.RecordGitHubPollFailureWithKind(ctx, repository, now, store.GitHubPollFailurePreActivationInboxConflict); err != nil {
					return err
				}
				return db.RecordGitHubPollFailure(ctx, repository, now.Add(time.Second))
			},
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx := context.Background()
			db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			repository := "owner/repo"
			failedAt := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)
			if err := scenario.record(ctx, db, repository, failedAt); err != nil {
				t.Fatal(err)
			}
			bootstrapRuns := 0
			_, err = (Poller{
				Store:       db,
				Client:      NewClient("http://example.invalid", "", nil).WithRepositoryOwner("owner"),
				MaxFailures: scenario.maxFailures,
				Now:         func() time.Time { return failedAt.Add(time.Minute) },
			}).PollWithBootstrap(ctx, repository, func(context.Context) error {
				bootstrapRuns++
				return nil
			}, func(context.Context, bool) error {
				t.Fatal("control pass ran after an unrecoverable failure")
				return nil
			})
			if !errors.Is(err, store.ErrNeedsAttention) {
				t.Fatalf("poll error = %v, want needs attention", err)
			}
			if bootstrapRuns != 0 {
				t.Fatalf("bootstrap runs = %d, want 0", bootstrapRuns)
			}
		})
	}
}

func TestPollDoesNotBootstrapPastFailureBudgetWithActivePlan(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := "owner/repo"
	activatePollerPlan(t, ctx, db, repository)
	failedAt := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)
	if err := db.RecordGitHubPollFailure(ctx, repository, failedAt); err != nil {
		t.Fatal(err)
	}
	bootstrapRuns := 0
	controlRuns := 0
	_, err = (Poller{
		Store:       db,
		Client:      NewClient("http://example.invalid", "", nil).WithRepositoryOwner("owner"),
		MaxFailures: 1,
		Now:         func() time.Time { return failedAt.Add(time.Minute) },
	}).PollWithBootstrap(ctx, repository, func(context.Context) error {
		bootstrapRuns++
		return nil
	}, func(context.Context, bool) error {
		controlRuns++
		return nil
	})
	if !errors.Is(err, store.ErrNeedsAttention) {
		t.Fatalf("poll error = %v, want needs attention", err)
	}
	if bootstrapRuns != 0 || controlRuns != 0 {
		t.Fatalf("bootstrap, control = %d, %d", bootstrapRuns, controlRuns)
	}
}

func activatePollerPlan(t *testing.T, ctx context.Context, db *store.Store, repository string) {
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
	version, err := db.BeginActivation(ctx, snapshot, fingerprint, "poller")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkActive(ctx, version.ID); err != nil {
		t.Fatal(err)
	}
}

func TestRecordFailurePersistsAfterParentCancellation(t *testing.T) {
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	now := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	_, err = (Poller{Store: db}).recordFailure(ctx, "owner/repo", now, context.DeadlineExceeded)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("record failure error = %v", err)
	}
	cursor, err := db.GitHubPollCursor(context.Background(), "owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	if cursor.ConsecutiveFailures != 1 {
		t.Fatalf("consecutive failures = %d, want 1", cursor.ConsecutiveFailures)
	}
}

func TestRecordFailureDefersRateLimitedAdmission(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC)
	retryAt := now.Add(time.Minute)
	_, err = (Poller{Store: db, Now: func() time.Time { return now }}).RecordFailure(ctx, "owner/repo", &apiError{StatusCode: 403, RetryAt: retryAt})
	if err == nil {
		t.Fatal("rate-limited admission failure returned nil")
	}
	cursor, err := db.GitHubPollCursor(ctx, "owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	if !cursor.NextAttemptAt.Equal(retryAt) || cursor.ConsecutiveFailures != 0 {
		t.Fatalf("cursor = %#v", cursor)
	}
}

func TestPollSkipsInboxWhileRateLimited(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	if err := db.DeferGitHubPoll(ctx, "owner/repo", now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	projector := &countingInboxProjector{}
	_, err = (Poller{Store: db, Client: NewClient("http://example.invalid", "", nil), InboxProjector: projector, Now: func() time.Time { return now }}).Poll(ctx, "owner/repo")
	if !errors.Is(err, store.ErrNotReady) {
		t.Fatalf("poll error = %v, want not ready", err)
	}
	if projector.calls != 0 {
		t.Fatalf("inbox projector calls = %d, want 0", projector.calls)
	}
}
