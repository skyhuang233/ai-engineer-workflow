package github

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
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

type errorInboxProjector struct {
	err error
}

func (p errorInboxProjector) ProjectWorkflowInbox(context.Context, string, []plan.WorkflowQuestion) error {
	return p.err
}

type failOnceInboxProjector struct {
	calls int
	err   error
}

func (p *failOnceInboxProjector) ProjectWorkflowInbox(context.Context, string, []plan.WorkflowQuestion) error {
	p.calls++
	if p.calls == 1 {
		return p.err
	}
	return nil
}

type completingConflictInboxProjector struct {
	store     *store.Store
	versionID string
	ticketID  int64
}

func (p completingConflictInboxProjector) ProjectWorkflowInbox(ctx context.Context, _ string, _ []plan.WorkflowQuestion) error {
	if err := p.store.MarkTicketDelivered(ctx, p.versionID, p.ticketID); err != nil {
		return err
	}
	return &delivery.HTTPError{StatusCode: http.StatusConflict, Code: delivery.ErrorCodeNoActiveDeliveryPlan, Message: "plan completed during dispatch"}
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
	}).PollWithBootstrap(ctx, repository, bootstrap, func(ctx context.Context, bootstrapped bool) (BootstrapControlResult, error) {
		if !bootstrapped {
			return BootstrapControlResult{}, bootstrap(ctx)
		}
		return BootstrapControlResult{}, nil
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

func TestPollSkipsInboxAndGitHubWorkWithoutPollablePlan(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	versionID := activatePollerPlan(t, ctx, db, "owner/repo")
	if err := db.MarkTicketDelivered(ctx, versionID, 2); err != nil {
		t.Fatal(err)
	}
	projector := &countingInboxProjector{}
	_, err = (Poller{
		Store:          db,
		Client:         NewClient("http://example.invalid", "", nil).WithRepositoryOwner("owner"),
		InboxProjector: projector,
		Now:            func() time.Time { return time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC) },
	}).Poll(ctx, "owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	if projector.calls != 0 {
		t.Fatalf("inbox projector calls = %d, want 0", projector.calls)
	}
}

func TestPollStoreFailureDoesNotConsumeRetryBudget(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "workflow.db")
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := "owner/repo"
	activatePollerPlan(t, ctx, db, repository)
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, "DROP TABLE ticket_deliveries"); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)
	_, err = (Poller{
		Store:          db,
		Client:         NewClient("http://example.invalid", "", nil).WithRepositoryOwner("owner"),
		InboxProjector: &countingInboxProjector{},
		MaxFailures:    1,
		Now:            func() time.Time { return now },
	}).Poll(ctx, repository)
	if err == nil || !errors.Is(err, ErrLocalPollStore) || errors.Is(err, store.ErrNeedsAttention) {
		t.Fatalf("store failure = %v, want fatal local-store error", err)
	}
	cursor, cursorErr := db.GitHubPollCursor(ctx, repository)
	if cursorErr != nil {
		t.Fatal(cursorErr)
	}
	if cursor.ConsecutiveFailures != 0 || cursor.NeedsAttention() {
		t.Fatalf("store failure consumed retry budget: %#v", cursor)
	}
}

func TestClassifyPollErrorLeavesGatewayStoreFailuresRetryable(t *testing.T) {
	err := ClassifyPollError(errors.Join(delivery.ErrGatewayStore, errors.New("remote SQLite unavailable")))
	if errors.Is(err, ErrLocalPollStore) {
		t.Fatalf("remote Gateway store failure classified as fatal local error: %v", err)
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
	projectingVersionID := beginProjectingPollerPlan(t, ctx, db, repository)
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
	}).PollWithBootstrap(ctx, repository, nil, func(context.Context, bool) (BootstrapControlResult, error) {
		return BootstrapControlResult{AttemptedPlanVersionID: projectingVersionID}, nil
	})
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
	if cursor.RecoveryState != store.GitHubPollRecoveryAvailable {
		t.Fatalf("recovery state = %q, want available", cursor.RecoveryState)
	}
	if cursor.RecoveryPlanVersionID == "" {
		t.Fatal("recovery cursor did not retain projecting plan provenance")
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
	}, func(_ context.Context, bootstrapped bool) (BootstrapControlResult, error) {
		controlRuns++
		if !bootstrapped {
			t.Fatal("recovered poll did not report bootstrap completion")
		}
		return BootstrapControlResult{}, nil
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
	if cursor.RecoveryState != store.GitHubPollRecoveryCompleted {
		t.Fatalf("recovery state = %q, want completed", cursor.RecoveryState)
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

func TestTransientFailureIsRetryableUntilBudgetExhaustion(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "workflow.db")
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	repository := "owner/repo"
	now := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)
	cause := errors.New("GitHub temporarily unavailable")
	poller := Poller{Store: db, MaxFailures: 2, Now: func() time.Time { return now }}
	if _, err := poller.RecordFailure(ctx, repository, cause); !errors.Is(err, cause) || errors.Is(err, store.ErrNeedsAttention) {
		db.Close()
		t.Fatalf("first transient failure = %v", err)
	}
	cursor, err := db.GitHubPollCursor(ctx, repository)
	if err != nil || cursor.NeedsAttention() || cursor.FailureKind != store.GitHubPollFailureRetryable || cursor.RecoveryState != store.GitHubPollRecoveryConsumed || cursor.ConsecutiveFailures != 1 {
		db.Close()
		t.Fatalf("retryable cursor = %#v, %v", cursor, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	poller = Poller{Store: restarted, MaxFailures: 2, Now: func() time.Time { return now.Add(time.Minute) }}
	if _, err := poller.RecordFailure(ctx, repository, cause); !errors.Is(err, store.ErrNeedsAttention) {
		t.Fatalf("exhausted transient failure = %v, want needs attention", err)
	}
	cursor, err = restarted.GitHubPollCursor(ctx, repository)
	if err != nil || !cursor.NeedsAttention() || cursor.ConsecutiveFailures != 2 {
		t.Fatalf("exhausted cursor = %#v, %v", cursor, err)
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

	poller := Poller{
		Store:          db,
		InboxProjector: projector,
		MaxFailures:    1,
		Now:            func() time.Time { return now },
	}
	leaseCtx, release, err := poller.AcquireLease(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	_, err = poller.recordFailureWithKind(leaseCtx, repository, now, store.GitHubPollFailurePreActivationInboxConflict, "", preActivationConflict)
	err = errors.Join(err, release())
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
			}, func(context.Context, bool) (BootstrapControlResult, error) {
				t.Fatal("control pass ran after an unrecoverable failure")
				return BootstrapControlResult{}, nil
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
	}, func(context.Context, bool) (BootstrapControlResult, error) {
		controlRuns++
		return BootstrapControlResult{}, nil
	})
	if !errors.Is(err, store.ErrNeedsAttention) {
		t.Fatalf("poll error = %v, want needs attention", err)
	}
	if bootstrapRuns != 0 || controlRuns != 0 {
		t.Fatalf("bootstrap, control = %d, %d", bootstrapRuns, controlRuns)
	}
}

func TestPollConsumesRecoveryAfterActivePlanReadFailure(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "workflow.db")
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := "owner/repo"
	failedAt := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)
	beginProjectingPollerPlan(t, ctx, db, repository)
	if err := db.RecordGitHubPollFailureWithKind(ctx, repository, failedAt, store.GitHubPollFailurePreActivationInboxConflict); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, "DROP TABLE plan_terminal_states"); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	bootstrapRuns := 0
	_, err = (Poller{
		Store:       db,
		Client:      NewClient("http://example.invalid", "", nil).WithRepositoryOwner("owner"),
		MaxFailures: 1,
		Now:         func() time.Time { return failedAt.Add(time.Minute) },
	}).PollWithBootstrap(ctx, repository, func(context.Context) error {
		bootstrapRuns++
		return nil
	}, nil)
	if err == nil || errors.Is(err, store.ErrNeedsAttention) {
		t.Fatalf("active plan read failure = %v, want retryable store error", err)
	}
	if bootstrapRuns != 0 {
		t.Fatalf("bootstrap runs = %d, want 0", bootstrapRuns)
	}
	cursor, err := db.GitHubPollCursor(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	if cursor.FailureKind != store.GitHubPollFailurePreActivationInboxConflict || cursor.RecoveryState != store.GitHubPollRecoveryAvailable {
		t.Fatalf("cursor provenance = %q/%q, want preserved recovery", cursor.FailureKind, cursor.RecoveryState)
	}
}

func TestPollCursorReadFailureDoesNotTerminalizeRecovery(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "workflow.db")
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := "owner/repo"
	failedAt := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)
	beginProjectingPollerPlan(t, ctx, db, repository)
	if err := db.RecordGitHubPollFailureWithKind(ctx, repository, failedAt, store.GitHubPollFailurePreActivationInboxConflict); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, "UPDATE github_poll_cursors SET next_attempt_at = 'invalid' WHERE repository = ?", repository); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	bootstrapRuns := 0
	_, err = (Poller{
		Store:       db,
		Client:      NewClient("http://example.invalid", "", nil).WithRepositoryOwner("owner"),
		MaxFailures: 1,
		Now:         func() time.Time { return failedAt.Add(time.Minute) },
	}).PollWithBootstrap(ctx, repository, func(context.Context) error {
		bootstrapRuns++
		return nil
	}, nil)
	if err == nil || errors.Is(err, store.ErrNeedsAttention) {
		t.Fatalf("cursor read failure = %v, want retryable non-terminal error", err)
	}
	if bootstrapRuns != 0 {
		t.Fatalf("bootstrap runs = %d, want 0", bootstrapRuns)
	}
	raw, err = sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var kind store.GitHubPollFailureKind
	var recovery store.GitHubPollRecoveryState
	if err := raw.QueryRowContext(ctx, "SELECT failure_kind, recovery_state FROM github_poll_cursors WHERE repository = ?", repository).Scan(&kind, &recovery); err != nil {
		t.Fatal(err)
	}
	if kind != store.GitHubPollFailurePreActivationInboxConflict || recovery != store.GitHubPollRecoveryAvailable {
		t.Fatalf("cursor provenance = %q/%q, want preserved pre-activation/available", kind, recovery)
	}
}

func TestClaimedBootstrapRecoveryResumesExactVersionAfterRestart(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "workflow.db")
	repository := "owner/repo"
	failedAt := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	versionID := beginProjectingPollerPlan(t, ctx, db, repository)
	if err := db.RecordGitHubPollFailureWithKind(ctx, repository, failedAt, store.GitHubPollFailurePreActivationInboxConflict); err != nil {
		db.Close()
		t.Fatal(err)
	}
	claimed, err := db.ClaimGitHubPollBootstrapRecovery(ctx, repository, 1, failedAt.Add(time.Second))
	if err != nil || !claimed {
		db.Close()
		t.Fatalf("claim = %t, %v", claimed, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	bootstrapRuns := 0
	_, err = (Poller{
		Store:       restarted,
		Client:      NewClient("http://example.invalid", "", nil).WithRepositoryOwner("owner"),
		MaxFailures: 1,
		Now:         func() time.Time { return failedAt.Add(time.Minute) },
	}).PollWithBootstrap(ctx, repository, func(context.Context) error {
		bootstrapRuns++
		return restarted.MarkActive(ctx, versionID)
	}, nil)
	if err != nil {
		t.Fatalf("restarted poll = %v", err)
	}
	if bootstrapRuns != 1 {
		t.Fatalf("bootstrap runs after restart = %d, want 1", bootstrapRuns)
	}
	cursor, err := restarted.GitHubPollCursor(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	if cursor.NeedsAttention() || cursor.RecoveryState != store.GitHubPollRecoveryCompleted || cursor.ConsecutiveFailures != 0 {
		t.Fatalf("recovery state = %#v, want completed", cursor)
	}
}

func TestClaimedBootstrapRecoveryRateLimitHasBoundedDeferrals(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := "owner/repo"
	failedAt := time.Date(2026, 8, 7, 0, 0, 0, 0, time.UTC)
	versionID := beginProjectingPollerPlan(t, ctx, db, repository)
	if err := db.RecordGitHubPollFailureWithKind(ctx, repository, failedAt, store.GitHubPollFailurePreActivationInboxConflict); err != nil {
		t.Fatal(err)
	}
	now := failedAt.Add(time.Minute)
	retryAt := now.Add(5 * time.Minute)
	rateLimit := &delivery.HTTPError{StatusCode: http.StatusInternalServerError, Message: "Gateway rate limited", RetryAt: retryAt}
	_, err = (Poller{
		Store:       db,
		Client:      NewClient("http://example.invalid", "", nil).WithRepositoryOwner("owner"),
		MaxFailures: 1,
		Now:         func() time.Time { return now },
	}).PollWithBootstrap(ctx, repository, func(context.Context) error {
		return rateLimit
	}, nil)
	if !errors.Is(err, rateLimit) || errors.Is(err, store.ErrNeedsAttention) {
		t.Fatalf("rate-limited recovery = %v", err)
	}
	cursor, err := db.GitHubPollCursor(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	if cursor.FailureKind != store.GitHubPollFailurePreActivationInboxConflict || cursor.RecoveryState != store.GitHubPollRecoveryClaimed || cursor.RecoveryPlanVersionID != versionID || cursor.ConsecutiveFailures != 2 || !cursor.NextAttemptAt.Equal(retryAt) {
		t.Fatalf("deferred recovery cursor = %#v", cursor)
	}
	now = retryAt
	secondRetryAt := now.Add(5 * time.Minute)
	secondRateLimit := &delivery.HTTPError{StatusCode: http.StatusInternalServerError, Message: "Gateway rate limited again", RetryAt: secondRetryAt}
	_, err = (Poller{
		Store:       db,
		Client:      NewClient("http://example.invalid", "", nil).WithRepositoryOwner("owner"),
		MaxFailures: 1,
		Now:         func() time.Time { return now },
	}).PollWithBootstrap(ctx, repository, func(context.Context) error {
		return secondRateLimit
	}, nil)
	if !errors.Is(err, secondRateLimit) || !errors.Is(err, store.ErrNeedsAttention) {
		t.Fatalf("exhausted rate-limited recovery = %v", err)
	}
	cursor, err = db.GitHubPollCursor(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	if !cursor.NeedsAttention() {
		t.Fatalf("exhausted recovery cursor = %#v", cursor)
	}
}

func TestClaimedBootstrapRecoveryStartsFreshBudgetAfterActivationProjectionFailure(t *testing.T) {
	for _, test := range []struct {
		name     string
		complete bool
	}{
		{name: "active"},
		{name: "completed", complete: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			repository := "owner/repo"
			failedAt := time.Date(2026, 8, 7, 0, 0, 0, 0, time.UTC)
			versionID := beginProjectingPollerPlan(t, ctx, db, repository)
			for range 2 {
				if err := db.RecordGitHubPollFailureWithKind(ctx, repository, failedAt, store.GitHubPollFailurePreActivationInboxConflict); err != nil {
					t.Fatal(err)
				}
			}
			projectionErr := errors.New("reconcile active plan root: Gateway unavailable")
			now := failedAt.Add(time.Minute)
			_, err = (Poller{
				Store:       db,
				Client:      NewClient("http://example.invalid", "", nil).WithRepositoryOwner("owner"),
				MaxFailures: 2,
				Now:         func() time.Time { return now },
			}).PollWithBootstrap(ctx, repository, func(context.Context) error {
				if err := db.MarkActive(ctx, versionID); err != nil {
					return err
				}
				if test.complete {
					if err := db.MarkTicketDelivered(ctx, versionID, 2); err != nil {
						return err
					}
				}
				return projectionErr
			}, nil)
			if !errors.Is(err, projectionErr) || errors.Is(err, store.ErrNeedsAttention) {
				t.Fatalf("post-activation projection failure = %v, want fresh retry", err)
			}
			cursor, err := db.GitHubPollCursor(ctx, repository)
			if err != nil {
				t.Fatal(err)
			}
			if cursor.ConsecutiveFailures != 1 || cursor.FailureKind != store.GitHubPollFailureRetryable || cursor.RecoveryPlanVersionID != versionID || cursor.NeedsAttention() {
				t.Fatalf("post-activation cursor = %#v, want fresh retry budget", cursor)
			}
			questions, err := db.OpenWorkflowQuestions(ctx, repository, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, question := range questions {
				if question.Kind == "poll_failure" {
					t.Fatalf("post-activation failure created recovery question: %#v", question)
				}
			}
		})
	}
}

func TestCompletedBootstrapProjectionExhaustionRetainsRecoveryOwner(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := "owner/repo"
	versionID := beginProjectingPollerPlan(t, ctx, db, repository)
	if err := db.MarkActive(ctx, versionID); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkTicketDelivered(ctx, versionID, 2); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 7, 1, 0, 0, 0, time.UTC)
	poller := Poller{Store: db, MaxFailures: 2, Now: func() time.Time { return now }}
	projectionErr := errors.New("reconcile completed plan root: Gateway unavailable")
	for attempt := range 2 {
		leaseCtx, release, err := poller.AcquireLease(ctx, repository)
		if err != nil {
			t.Fatal(err)
		}
		_, failureErr := poller.recordBootstrapFailure(leaseCtx, repository, now, versionID, projectionErr)
		failureErr = errors.Join(failureErr, release())
		if !errors.Is(failureErr, projectionErr) {
			t.Fatalf("projection failure %d = %v", attempt+1, failureErr)
		}
		if attempt == 0 && errors.Is(failureErr, store.ErrNeedsAttention) {
			t.Fatalf("first projection failure exhausted fresh budget: %v", failureErr)
		}
		if attempt == 1 && !errors.Is(failureErr, store.ErrNeedsAttention) {
			t.Fatalf("second projection failure = %v, want Needs Attention", failureErr)
		}
		now = now.Add(2 * time.Second)
	}
	cursor, err := db.GitHubPollCursor(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	if !cursor.NeedsAttention() || cursor.RecoveryPlanVersionID != versionID {
		t.Fatalf("terminal completed-plan cursor = %#v", cursor)
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
		t.Fatalf("completed-plan recovery questions = %#v", questions)
	}
}

func TestClaimedBootstrapRecoveryIgnoresUnrelatedProjectingPlan(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := "owner/repo"
	failedAt := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)
	versionID := beginProjectingPollerPlanAt(t, ctx, db, repository, 1, 1, 2, 2)
	if err := db.RecordGitHubPollFailureWithKind(ctx, repository, failedAt, store.GitHubPollFailurePreActivationInboxConflict); err != nil {
		t.Fatal(err)
	}
	claimed, err := db.ClaimGitHubPollBootstrapRecovery(ctx, repository, 1, failedAt.Add(time.Second))
	if err != nil || !claimed {
		t.Fatalf("claim = %t, %v", claimed, err)
	}
	beginProjectingPollerPlanAt(t, ctx, db, repository, 11, 11, 12, 12)
	now := failedAt.Add(time.Minute)
	poller := Poller{Store: db, MaxFailures: 1, Now: func() time.Time { return now }}
	leaseCtx, release, err := poller.AcquireLease(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := release(); err != nil {
			t.Error(err)
		}
	}()
	cursor, err := db.GitHubPollCursor(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	bootstrapRuns := 0
	recovered, err := poller.resumeClaimedBootstrapRecovery(leaseCtx, repository, cursor, func(context.Context) error {
		bootstrapRuns++
		return db.MarkActive(ctx, versionID)
	}, now, leaseCtx.Value(githubPollLeaseContextKey{}).(githubPollLease).token)
	if err != nil || !recovered || bootstrapRuns != 1 {
		t.Fatalf("recovery = %t runs=%d error=%v", recovered, bootstrapRuns, err)
	}
}

func TestActiveClaimedBootstrapRecoveryResumesWithoutRepeatingBootstrap(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "workflow.db")
	repository := "owner/repo"
	failedAt := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	versionID := beginProjectingPollerPlan(t, ctx, db, repository)
	if err := db.RecordGitHubPollFailureWithKind(ctx, repository, failedAt, store.GitHubPollFailurePreActivationInboxConflict); err != nil {
		db.Close()
		t.Fatal(err)
	}
	claimed, err := db.ClaimGitHubPollBootstrapRecovery(ctx, repository, 1, failedAt.Add(time.Second))
	if err != nil || !claimed {
		db.Close()
		t.Fatalf("claim = %t, %v", claimed, err)
	}
	if err := db.MarkActive(ctx, versionID); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	now := failedAt.Add(time.Minute)
	projector := &activePlanInboxProjector{store: restarted, now: now}
	bootstrapRuns := 0
	_, err = (Poller{
		Store:          restarted,
		Client:         NewClient("http://example.invalid", "", nil).WithRepositoryOwner("owner"),
		InboxProjector: projector,
		MaxFailures:    1,
		Now:            func() time.Time { return now },
	}).PollWithBootstrap(ctx, repository, func(context.Context) error {
		bootstrapRuns++
		return nil
	}, nil)
	if err != nil {
		t.Fatalf("restarted active poll = %v", err)
	}
	if bootstrapRuns != 0 {
		t.Fatalf("bootstrap runs after active restart = %d, want 0", bootstrapRuns)
	}
	if projector.calls != 1 {
		t.Fatalf("inbox projections = %d, want 1", projector.calls)
	}
	questions, err := restarted.OpenWorkflowQuestions(ctx, repository, 0)
	if err != nil {
		t.Fatal(err)
	}
	pollFailureVisible := false
	for _, question := range questions {
		if question.Kind == "poll_failure" {
			pollFailureVisible = true
			break
		}
	}
	if pollFailureVisible {
		t.Fatalf("unexpected human recovery questions = %#v", questions)
	}
	cursor, err := restarted.GitHubPollCursor(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	if cursor.ConsecutiveFailures != 0 || cursor.RecoveryState != store.GitHubPollRecoveryCompleted {
		t.Fatalf("recovery cursor = %#v, want completed with reset budget", cursor)
	}
}

func TestAvailableBootstrapRecoveryCompletesForBoundActivePlan(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := "owner/repo"
	failedAt := time.Now().UTC().Add(-time.Minute)
	versionID := beginProjectingPollerPlan(t, ctx, db, repository)
	if err := db.RecordGitHubPollFailureWithKind(ctx, repository, failedAt, store.GitHubPollFailurePreActivationInboxConflict); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkActive(ctx, versionID); err != nil {
		t.Fatal(err)
	}
	bootstrapRuns := 0
	_, err = (Poller{
		Store:          db,
		Client:         NewClient("http://example.invalid", "", nil).WithRepositoryOwner("owner"),
		InboxProjector: &countingInboxProjector{},
		MaxFailures:    1,
		Now:            func() time.Time { return failedAt.Add(time.Minute) },
	}).PollWithBootstrap(ctx, repository, func(context.Context) error {
		bootstrapRuns++
		return nil
	}, nil)
	if err != nil {
		t.Fatalf("bound active recovery = %v", err)
	}
	if bootstrapRuns != 0 {
		t.Fatalf("bootstrap runs = %d, want 0", bootstrapRuns)
	}
	cursor, err := db.GitHubPollCursor(ctx, repository)
	if err != nil || cursor.NeedsAttention() || cursor.RecoveryState != store.GitHubPollRecoveryCompleted {
		t.Fatalf("bound active cursor = %#v, %v", cursor, err)
	}
}

func TestStaleClaimedRecoveryPreservesReplacementWorker(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := "owner/repo"
	failedAt := time.Now().UTC().Add(-time.Minute)
	originVersionID := beginProjectingPollerPlan(t, ctx, db, repository)
	if err := db.RecordGitHubPollFailureWithKind(ctx, repository, failedAt, store.GitHubPollFailurePreActivationInboxConflict); err != nil {
		t.Fatal(err)
	}
	claimed, err := db.ClaimGitHubPollBootstrapRecovery(ctx, repository, 1, failedAt.Add(time.Second))
	if err != nil || !claimed {
		t.Fatalf("claim = %t, %v", claimed, err)
	}
	if err := db.MarkActive(ctx, originVersionID); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkTicketDelivered(ctx, originVersionID, 2); err != nil {
		t.Fatal(err)
	}
	replacement := plan.Snapshot{
		Repository: repository,
		Root:       plan.Issue{ID: 10, Number: 10, Labels: []string{plan.PlanLabel}},
		Children:   []plan.Issue{{ID: 20, Number: 20, Labels: []string{plan.TicketLabel}, State: "open"}},
	}
	fingerprint, err := replacement.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	replacementVersion, err := db.BeginActivation(ctx, replacement, fingerprint, "replacement")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkActive(ctx, replacementVersion.ID); err != nil {
		t.Fatal(err)
	}
	now := failedAt.Add(time.Minute)
	worker, err := db.ClaimReady(ctx, store.ClaimRequest{VersionID: replacementVersion.ID, TicketID: 20, Owner: "worker", MaxParallelRuns: 1, LeaseTTL: time.Hour, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	bootstrapRuns := 0
	_, err = (Poller{
		Store:          db,
		Client:         NewClient("http://example.invalid", "", nil).WithRepositoryOwner("owner"),
		InboxProjector: &countingInboxProjector{},
		MaxFailures:    1,
		Now:            func() time.Time { return now },
	}).PollWithBootstrap(ctx, repository, func(context.Context) error {
		bootstrapRuns++
		return nil
	}, nil)
	if err != nil {
		t.Fatalf("stale recovery poll = %v", err)
	}
	if bootstrapRuns != 0 {
		t.Fatalf("stale bootstrap runs = %d, want 0", bootstrapRuns)
	}
	current, err := db.CurrentClaim(ctx, replacementVersion.ID, 20)
	if err != nil || current.RunID != worker.RunID {
		t.Fatalf("replacement worker = %#v, %v; want run %q", current, err, worker.RunID)
	}
	cursor, err := db.GitHubPollCursor(ctx, repository)
	if err != nil || cursor.NeedsAttention() || cursor.ConsecutiveFailures != 0 {
		t.Fatalf("stale recovery cursor = %#v, %v", cursor, err)
	}
}

func TestClaimedBootstrapRecoveryRetriesAfterDatabaseFailure(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := "owner/repo"
	failedAt := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)
	beginProjectingPollerPlan(t, ctx, db, repository)
	if err := db.RecordGitHubPollFailureWithKind(ctx, repository, failedAt, store.GitHubPollFailurePreActivationInboxConflict); err != nil {
		t.Fatal(err)
	}
	broken, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "broken.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer broken.Close()
	var ignored int
	databaseErr := broken.QueryRowContext(ctx, "SELECT value FROM missing_table").Scan(&ignored)
	if databaseErr == nil {
		t.Fatal("missing table query succeeded")
	}
	now := failedAt.Add(time.Minute)
	poller := Poller{
		Store:       db,
		Client:      NewClient("http://example.invalid", "", nil).WithRepositoryOwner("owner"),
		MaxFailures: 1,
		Now:         func() time.Time { return now },
	}
	_, err = poller.PollWithBootstrap(ctx, repository, func(context.Context) error {
		return fmt.Errorf("read bootstrap state: %w", databaseErr)
	}, nil)
	if !errors.Is(err, databaseErr) || errors.Is(err, store.ErrNeedsAttention) {
		t.Fatalf("bootstrap database failure = %v, want retryable database error", err)
	}
	cursor, err := db.GitHubPollCursor(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	if cursor.FailureKind != store.GitHubPollFailurePreActivationInboxConflict || cursor.RecoveryState != store.GitHubPollRecoveryClaimed {
		t.Fatalf("bootstrap database failure changed recovery = %#v", cursor)
	}
	_, err = poller.PollWithBootstrap(ctx, repository, func(ctx context.Context) error {
		activatePollerPlan(t, ctx, db, repository)
		return nil
	}, nil)
	if err != nil {
		t.Fatalf("resumed bootstrap recovery = %v", err)
	}
	cursor, err = db.GitHubPollCursor(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	if cursor.ConsecutiveFailures != 0 || cursor.RecoveryState != store.GitHubPollRecoveryCompleted {
		t.Fatalf("resumed recovery cursor = %#v", cursor)
	}
}

func TestInboxConflictProvenanceReadFailureRemainsRetryable(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "workflow.db")
	repository := "owner/repo"
	failedAt := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	beginProjectingPollerPlan(t, ctx, db, repository)
	if err := db.RecordGitHubPollFailureWithKind(ctx, repository, failedAt, store.GitHubPollFailurePreActivationInboxConflict); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, "DROP TABLE plan_terminal_states"); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	gatewayErr := &delivery.HTTPError{StatusCode: http.StatusConflict, Code: delivery.ErrorCodeNoActiveDeliveryPlan, Message: "no active delivery plan"}
	_, err = (Poller{
		Store:          db,
		Client:         NewClient("http://example.invalid", "", nil).WithRepositoryOwner("owner"),
		InboxProjector: errorInboxProjector{err: gatewayErr},
		MaxFailures:    2,
		Now:            func() time.Time { return failedAt.Add(time.Minute) },
	}).Poll(ctx, repository)
	if err == nil || errors.Is(err, store.ErrNeedsAttention) {
		t.Fatalf("provenance read failure = %v, want retryable store error", err)
	}
	if !strings.Contains(err.Error(), "plan_terminal_states") {
		t.Fatalf("classification database cause missing from %v", err)
	}
	cursor, err := db.GitHubPollCursor(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	if cursor.FailureKind != store.GitHubPollFailurePreActivationInboxConflict || cursor.RecoveryState != store.GitHubPollRecoveryAvailable {
		t.Fatalf("cursor provenance = %q/%q, want preserved recovery", cursor.FailureKind, cursor.RecoveryState)
	}
}

func TestBootstrapRecoveryClaimHasOneWinner(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := "owner/repo"
	now := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)
	beginProjectingPollerPlan(t, ctx, db, repository)
	if err := db.RecordGitHubPollFailureWithKind(ctx, repository, now, store.GitHubPollFailurePreActivationInboxConflict); err != nil {
		t.Fatal(err)
	}
	leaseNow := now.Add(time.Second)
	if err := db.AcquireGitHubPollLease(ctx, repository, "recovery-test", leaseNow, time.Minute); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.ReleaseGitHubPollLease(ctx, repository, "recovery-test", leaseNow); err != nil {
			t.Error(err)
		}
	}()
	start := make(chan struct{})
	results := make(chan bool, 2)
	errorsFound := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			claimed, err := db.ClaimGitHubPollBootstrapRecoveryLeased(ctx, repository, 1, leaseNow, "recovery-test", leaseNow)
			if err != nil {
				errorsFound <- err
				return
			}
			results <- claimed
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	close(errorsFound)
	for err := range errorsFound {
		t.Fatal(err)
	}
	winners := 0
	for claimed := range results {
		if claimed {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("claim winners = %d, want 1", winners)
	}
}

func TestBootstrapRecoveryClaimRejectsCompletedOriginPlan(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := "owner/repo"
	now := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)
	versionID := beginProjectingPollerPlan(t, ctx, db, repository)
	if err := db.RecordGitHubPollFailureWithKind(ctx, repository, now, store.GitHubPollFailurePreActivationInboxConflict); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkActive(ctx, versionID); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkTicketDelivered(ctx, versionID, 2); err != nil {
		t.Fatal(err)
	}
	claimed, err := db.ClaimGitHubPollBootstrapRecovery(ctx, repository, 1, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if claimed {
		t.Fatal("completed originating plan retained bootstrap recovery authority")
	}
}

func TestTerminalFailureRemainsPausedAcrossRestartBelowRetryBudget(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "workflow.db")
	repository := "owner/repo"
	now := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	cause := errors.New("control database read failed")
	_, err = (Poller{Store: db, MaxFailures: 5, Now: func() time.Time { return now }}).RecordTerminalFailure(ctx, repository, cause)
	if !errors.Is(err, cause) || !errors.Is(err, store.ErrNeedsAttention) {
		t.Fatalf("terminal failure = %v, want cause and needs attention", err)
	}
	cursor, err := db.GitHubPollCursor(ctx, repository)
	if err != nil || !cursor.NeedsAttention() || cursor.ConsecutiveFailures >= 5 {
		t.Fatalf("terminal cursor = %#v, %v", cursor, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	bootstrapRuns := 0
	_, err = (Poller{
		Store:       restarted,
		Client:      NewClient("http://example.invalid", "", nil).WithRepositoryOwner("owner"),
		MaxFailures: 5,
		Now:         func() time.Time { return now.Add(time.Minute) },
	}).PollWithBootstrap(ctx, repository, func(context.Context) error {
		bootstrapRuns++
		return nil
	}, nil)
	if !errors.Is(err, store.ErrNeedsAttention) {
		t.Fatalf("restarted terminal poll = %v, want needs attention", err)
	}
	if bootstrapRuns != 0 {
		t.Fatalf("bootstrap runs after terminal restart = %d, want 0", bootstrapRuns)
	}
	cursor, err = restarted.GitHubPollCursor(ctx, repository)
	if err != nil || !cursor.NeedsAttention() {
		t.Fatalf("restarted terminal cursor = %#v, %v", cursor, err)
	}
}

func TestTerminalFailureProjectsRecoveryForActivePlanBelowRetryBudget(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := "owner/repo"
	activatePollerPlan(t, ctx, db, repository)
	now := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)
	projector := &activePlanInboxProjector{store: db, now: now}
	_, err = (Poller{Store: db, InboxProjector: projector, MaxFailures: 5, Now: func() time.Time { return now }}).RecordTerminalFailure(ctx, repository, errors.New("control failure"))
	if !errors.Is(err, store.ErrNeedsAttention) {
		t.Fatalf("active terminal failure = %v, want needs attention", err)
	}
	if projector.calls != 1 {
		t.Fatalf("active terminal projections = %d, want 1", projector.calls)
	}
	questions, err := db.OpenWorkflowQuestions(ctx, repository, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, question := range questions {
		found = found || question.Kind == "poll_failure"
	}
	if !found {
		t.Fatalf("active terminal recovery questions = %#v", questions)
	}
}

func TestNeedsAttentionPollRetriesRecoveryInboxProjection(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := "owner/repo"
	activatePollerPlan(t, ctx, db, repository)
	now := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)
	projectionErr := errors.New("Gateway temporarily unavailable")
	projector := &failOnceInboxProjector{err: projectionErr}
	poller := Poller{
		Store:          db,
		Client:         NewClient("http://example.invalid", "", nil).WithRepositoryOwner("owner"),
		InboxProjector: projector,
		Now:            func() time.Time { return now },
	}
	_, err = poller.RecordTerminalFailure(ctx, repository, errors.New("GitHub polling exhausted"))
	if !errors.Is(err, store.ErrNeedsAttention) || !errors.Is(err, projectionErr) {
		t.Fatalf("terminal projection failure = %v", err)
	}
	_, err = poller.Poll(ctx, repository)
	if !errors.Is(err, store.ErrNeedsAttention) || errors.Is(err, projectionErr) {
		t.Fatalf("retried recovery projection = %v", err)
	}
	if projector.calls != 2 {
		t.Fatalf("recovery projection calls = %d, want 2", projector.calls)
	}
}

func activatePollerPlan(t *testing.T, ctx context.Context, db *store.Store, repository string) string {
	t.Helper()
	versionID := beginProjectingPollerPlan(t, ctx, db, repository)
	if err := db.MarkActive(ctx, versionID); err != nil {
		t.Fatal(err)
	}
	return versionID
}

func TestCompletedOrAbsentPlanInboxConflictIsNotBootstrapEligible(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cause := &delivery.HTTPError{StatusCode: http.StatusConflict, Code: delivery.ErrorCodeNoActiveDeliveryPlan}
	kind, _, ignored, err := (Poller{Store: db}).inboxProjectionFailureKind(ctx, "owner/repo", nil, "", cause)
	if err != nil {
		t.Fatal(err)
	}
	if kind != "" || !ignored {
		t.Fatalf("absent-plan failure kind = %q ignored=%t, want ignored", kind, ignored)
	}
	completedVersion := activatePollerPlan(t, ctx, db, "owner/completed")
	if err := db.MarkTicketDelivered(ctx, completedVersion, 2); err != nil {
		t.Fatal(err)
	}
	kind, _, ignored, err = (Poller{Store: db}).inboxProjectionFailureKind(ctx, "owner/completed", []string{completedVersion}, "", cause)
	if err != nil || kind != "" || !ignored {
		t.Fatalf("completed-plan failure kind = %q ignored=%t, %v; want ignored", kind, ignored, err)
	}
	projectingVersionID := beginProjectingPollerPlan(t, ctx, db, "owner/projecting")
	kind, provenance, ignored, err := (Poller{Store: db}).inboxProjectionFailureKind(ctx, "owner/projecting", nil, projectingVersionID, cause)
	if err != nil || kind != store.GitHubPollFailurePreActivationInboxConflict {
		t.Fatalf("projecting-plan failure kind = %q, %v", kind, err)
	}
	if provenance != projectingVersionID {
		t.Fatalf("projecting-plan provenance = %q, want %q", provenance, projectingVersionID)
	}
	if ignored {
		t.Fatal("projecting-plan conflict was ignored")
	}
}

func TestInboxConflictUsesControlPassPlanProvenance(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := "owner/repo"
	unrelatedVersionID := beginProjectingPollerPlan(t, ctx, db, repository)
	attemptedVersionID := beginProjectingPollerPlanAt(t, ctx, db, repository, 11, 11, 12, 12)
	if err := db.MarkActive(ctx, attemptedVersionID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	conflict := &delivery.HTTPError{StatusCode: http.StatusConflict, Code: delivery.ErrorCodeNoActiveDeliveryPlan}
	_, err = (Poller{
		Store:          db,
		Client:         NewClient("http://example.invalid", "", nil).WithRepositoryOwner("owner"),
		InboxProjector: errorInboxProjector{err: conflict},
		MaxFailures:    1,
		Now:            func() time.Time { return now },
	}).PollWithBootstrap(ctx, repository, nil, func(context.Context, bool) (BootstrapControlResult, error) {
		if err := db.MarkTicketDelivered(ctx, attemptedVersionID, 12); err != nil {
			return BootstrapControlResult{}, err
		}
		return BootstrapControlResult{AttemptedPlanVersionID: attemptedVersionID}, nil
	})
	if err != nil {
		t.Fatalf("completed attempted Plan conflict = %v, want ignored", err)
	}
	cursor, err := db.GitHubPollCursor(ctx, repository)
	if err != nil || cursor.ConsecutiveFailures != 0 || cursor.RecoveryPlanVersionID != "" || cursor.NeedsAttention() {
		t.Fatalf("poll cursor = %#v, %v", cursor, err)
	}
	unrelated, err := db.CurrentVersion(ctx, repository, 1)
	if err != nil || unrelated.ID != unrelatedVersionID || unrelated.State != store.StateProjecting {
		t.Fatalf("unrelated Plan = %#v, %v", unrelated, err)
	}
}

func TestBootstrapFailurePreservesAttemptedPlanThroughRecoveryTerminalization(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := "owner/repo"
	unrelatedVersionID := beginProjectingPollerPlan(t, ctx, db, repository)
	attemptedVersionID := beginProjectingPollerPlanAt(t, ctx, db, repository, 11, 11, 12, 12)
	now := time.Date(2026, 8, 7, 0, 0, 0, 0, time.UTC)
	projectionErr := errors.New("project Plan root: Gateway unavailable")
	poller := Poller{
		Store:       db,
		Client:      NewClient("http://example.invalid", "", nil).WithRepositoryOwner("owner"),
		MaxFailures: 1,
		Now:         func() time.Time { return now },
	}
	_, err = poller.PollWithBootstrap(ctx, repository, func(context.Context) error {
		return projectionErr
	}, func(context.Context, bool) (BootstrapControlResult, error) {
		return BootstrapControlResult{AttemptedPlanVersionID: attemptedVersionID}, projectionErr
	})
	if !errors.Is(err, projectionErr) || !errors.Is(err, store.ErrNeedsAttention) {
		t.Fatalf("initial bootstrap failure = %v, want retryable recovery", err)
	}
	cursor, err := db.GitHubPollCursor(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	if cursor.FailureKind != store.GitHubPollFailurePreActivationInboxConflict || cursor.RecoveryState != store.GitHubPollRecoveryAvailable || cursor.RecoveryPlanVersionID != attemptedVersionID {
		t.Fatalf("initial bootstrap cursor = %#v, want attempted Plan recovery", cursor)
	}
	now = cursor.NextAttemptAt
	_, err = poller.PollWithBootstrap(ctx, repository, func(context.Context) error {
		return projectionErr
	}, nil)
	if !errors.Is(err, projectionErr) || !errors.Is(err, store.ErrNeedsAttention) {
		t.Fatalf("resumed bootstrap failure = %v, want terminal recovery", err)
	}
	cursor, err = db.GitHubPollCursor(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	if !cursor.NeedsAttention() || cursor.RecoveryPlanVersionID != attemptedVersionID {
		t.Fatalf("terminal bootstrap cursor = %#v, want attempted Plan provenance", cursor)
	}
	questions, err := db.OpenWorkflowQuestions(ctx, repository, 0)
	if err != nil {
		t.Fatal(err)
	}
	foundAttempted := false
	for _, question := range questions {
		foundAttempted = foundAttempted || question.VersionID == attemptedVersionID && question.Kind == "poll_failure"
		if question.VersionID == unrelatedVersionID && question.Kind == "poll_failure" {
			t.Fatalf("unrelated Plan received recovery question: %#v", question)
		}
	}
	if !foundAttempted {
		t.Fatalf("workflow questions = %#v, want attempted Plan poll failure", questions)
	}
}

func TestMixedBootstrapFailureTerminalizesAttemptedPlan(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := "owner/repo"
	unrelatedVersionID := beginProjectingPollerPlan(t, ctx, db, repository)
	attemptedVersionID := beginProjectingPollerPlanAt(t, ctx, db, repository, 11, 11, 12, 12)
	failedAt := time.Date(2026, 8, 7, 0, 0, 0, 0, time.UTC)
	if err := db.RecordGitHubPollFailure(ctx, repository, failedAt); err != nil {
		t.Fatal(err)
	}
	projectionErr := errors.New("project Plan root: Gateway unavailable")
	_, err = (Poller{
		Store:       db,
		Client:      NewClient("http://example.invalid", "", nil).WithRepositoryOwner("owner"),
		MaxFailures: 2,
		Now:         func() time.Time { return failedAt.Add(time.Minute) },
	}).PollWithBootstrap(ctx, repository, nil, func(context.Context, bool) (BootstrapControlResult, error) {
		return BootstrapControlResult{AttemptedPlanVersionID: attemptedVersionID}, projectionErr
	})
	if !errors.Is(err, projectionErr) || !errors.Is(err, store.ErrNeedsAttention) {
		t.Fatalf("mixed bootstrap failure = %v", err)
	}
	cursor, err := db.GitHubPollCursor(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	if !cursor.NeedsAttention() || cursor.RecoveryPlanVersionID != attemptedVersionID {
		t.Fatalf("terminal cursor = %#v", cursor)
	}
	questions, err := db.OpenWorkflowQuestions(ctx, repository, 0)
	if err != nil {
		t.Fatal(err)
	}
	foundAttempted := false
	for _, question := range questions {
		foundAttempted = foundAttempted || question.VersionID == attemptedVersionID && question.Kind == "poll_failure"
		if question.VersionID == unrelatedVersionID && question.Kind == "poll_failure" {
			t.Fatalf("unrelated Plan received recovery question: %#v", question)
		}
	}
	if !foundAttempted {
		t.Fatalf("workflow questions = %#v", questions)
	}
}

func TestActivePlanCompletionRaceDoesNotConsumePollFailureBudget(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := "owner/repo"
	versionID := activatePollerPlan(t, ctx, db, repository)
	now := time.Now().UTC()
	_, err = (Poller{
		Store:          db,
		Client:         NewClient("http://example.invalid", "", nil).WithRepositoryOwner("owner"),
		InboxProjector: completingConflictInboxProjector{store: db, versionID: versionID, ticketID: 2},
		Now:            func() time.Time { return now },
	}).Poll(ctx, repository)
	if err != nil {
		t.Fatalf("completion race poll = %v", err)
	}
	cursor, err := db.GitHubPollCursor(ctx, repository)
	if err != nil || cursor.ConsecutiveFailures != 0 || cursor.NeedsAttention() {
		t.Fatalf("completion race cursor = %#v, %v", cursor, err)
	}
}

func TestMultipleActivePlanCompletionRaceDoesNotConsumePollFailureBudget(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := "owner/repo"
	activatePollerPlan(t, ctx, db, repository)
	completedVersionID := beginProjectingPollerPlanAt(t, ctx, db, repository, 11, 11, 12, 12)
	if err := db.MarkActive(ctx, completedVersionID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	_, err = (Poller{
		Store:          db,
		Client:         NewClient("http://example.invalid", "", nil).WithRepositoryOwner("owner"),
		InboxProjector: completingConflictInboxProjector{store: db, versionID: completedVersionID, ticketID: 12},
		Now:            func() time.Time { return now },
	}).Poll(ctx, repository)
	if err != nil {
		t.Fatalf("multiple-plan completion race poll = %v", err)
	}
	cursor, err := db.GitHubPollCursor(ctx, repository)
	if err != nil || cursor.ConsecutiveFailures != 0 || cursor.NeedsAttention() {
		t.Fatalf("multiple-plan completion race cursor = %#v, %v", cursor, err)
	}
}

func TestGatewayStoreProjectionFailurePersistsBoundedBackoff(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := "owner/repo"
	activatePollerPlan(t, ctx, db, repository)
	now := time.Now().UTC()
	gatewayStoreErr := &delivery.HTTPError{StatusCode: http.StatusInternalServerError, Code: delivery.ErrorCodeRetryableStore, Message: "database temporarily unavailable"}
	_, err = (Poller{
		Store:          db,
		Client:         NewClient("http://example.invalid", "", nil).WithRepositoryOwner("owner"),
		InboxProjector: errorInboxProjector{err: gatewayStoreErr},
		Now:            func() time.Time { return now },
	}).Poll(ctx, repository)
	if !errors.Is(err, gatewayStoreErr) {
		t.Fatalf("Gateway store poll error = %v", err)
	}
	cursor, err := db.GitHubPollCursor(ctx, repository)
	if err != nil || cursor.ConsecutiveFailures != 1 || cursor.NeedsAttention() || !cursor.NextAttemptAt.After(now) {
		t.Fatalf("Gateway store cursor = %#v, %v", cursor, err)
	}
}

func TestPrepareAdmissionResolvesBoundRecoveryBeforeRateLimit(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := "owner/repo"
	failedAt := time.Now().UTC().Add(-time.Minute)
	versionID := beginProjectingPollerPlan(t, ctx, db, repository)
	for range 2 {
		if err := db.RecordGitHubPollFailureWithKind(ctx, repository, failedAt, store.GitHubPollFailurePreActivationInboxConflict); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.MarkActive(ctx, versionID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	poller := Poller{Store: db, MaxFailures: 2, Now: func() time.Time { return now }}
	leaseCtx, release, err := poller.AcquireLease(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := release(); err != nil {
			t.Error(err)
		}
	}()
	if err := poller.PrepareAdmission(leaseCtx, repository); err != nil {
		t.Fatal(err)
	}
	rateLimit := &APIError{StatusCode: http.StatusForbidden, RetryAt: now.Add(time.Minute)}
	if _, err := poller.RecordAdmissionFailure(leaseCtx, repository, rateLimit); !errors.Is(err, rateLimit) || errors.Is(err, store.ErrNeedsAttention) {
		t.Fatalf("rate-limited admission = %v", err)
	}
	cursor, err := db.GitHubPollCursor(ctx, repository)
	if err != nil || cursor.NeedsAttention() || cursor.ConsecutiveFailures != 1 || cursor.RecoveryPlanVersionID != "" {
		t.Fatalf("post-admission cursor = %#v, %v", cursor, err)
	}
}

func TestRateLimitedAdmissionRecoveryClaimHasBoundedDeferrals(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := "owner/repo"
	failedAt := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)
	versionID := beginProjectingPollerPlan(t, ctx, db, repository)
	if err := db.RecordGitHubPollFailureWithKind(ctx, repository, failedAt, store.GitHubPollFailurePreActivationInboxConflict); err != nil {
		t.Fatal(err)
	}
	now := failedAt.Add(time.Minute)
	retryAt := now.Add(time.Second)
	poller := Poller{Store: db, MaxFailures: 1, Now: func() time.Time { return now }}
	leaseCtx, release, err := poller.AcquireLease(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := release(); err != nil {
			t.Error(err)
		}
	}()
	if err := poller.PrepareAdmission(leaseCtx, repository); err != nil {
		t.Fatal(err)
	}
	rateLimit := &APIError{StatusCode: http.StatusForbidden, RetryAt: retryAt}
	if _, err := poller.RecordAdmissionFailure(leaseCtx, repository, rateLimit); !errors.Is(err, rateLimit) || errors.Is(err, store.ErrNeedsAttention) {
		t.Fatalf("rate-limited admission = %v", err)
	}
	cursor, err := db.GitHubPollCursor(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	if cursor.RecoveryPlanVersionID != versionID || cursor.RecoveryState != store.GitHubPollRecoveryClaimed || cursor.ConsecutiveFailures != 2 || !cursor.NextAttemptAt.Equal(retryAt) {
		t.Fatalf("preserved recovery cursor = %#v", cursor)
	}
	now = retryAt
	secondRateLimit := &APIError{StatusCode: http.StatusForbidden, RetryAt: now.Add(time.Second)}
	if _, err := poller.RecordAdmissionFailure(leaseCtx, repository, secondRateLimit); !errors.Is(err, secondRateLimit) || !errors.Is(err, store.ErrNeedsAttention) {
		t.Fatalf("exhausted rate-limited admission = %v", err)
	}
	cursor, err = db.GitHubPollCursor(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	if !cursor.NeedsAttention() {
		t.Fatalf("exhausted recovery cursor = %#v", cursor)
	}
}

func TestPermanentAdmissionFailureTerminalizesExhaustedRecovery(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := "owner/repo"
	failedAt := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)
	versionID := beginProjectingPollerPlan(t, ctx, db, repository)
	if err := db.RecordGitHubPollFailureWithKind(ctx, repository, failedAt, store.GitHubPollFailurePreActivationInboxConflict); err != nil {
		t.Fatal(err)
	}
	now := failedAt.Add(time.Minute)
	poller := Poller{Store: db, MaxFailures: 1, Now: func() time.Time { return now }}
	leaseCtx, release, err := poller.AcquireLease(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := release(); err != nil {
			t.Error(err)
		}
	}()
	if err := poller.PrepareAdmission(leaseCtx, repository); err != nil {
		t.Fatal(err)
	}
	ownerMismatch := errors.New("repository owner does not match configured owner")
	if _, err := poller.RecordAdmissionFailure(leaseCtx, repository, ownerMismatch); !errors.Is(err, ownerMismatch) || !errors.Is(err, store.ErrNeedsAttention) {
		t.Fatalf("permanent admission = %v", err)
	}
	cursor, err := db.GitHubPollCursor(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	if cursor.FailureKind != store.GitHubPollFailureUnrecoverable || cursor.RecoveryState != store.GitHubPollRecoveryConsumed {
		t.Fatalf("terminal admission cursor = %#v", cursor)
	}
	questions, err := db.OpenWorkflowQuestions(ctx, repository, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, question := range questions {
		if question.VersionID == versionID && question.Kind == "poll_failure" {
			found = true
		}
	}
	if !found {
		t.Fatalf("projecting recovery Plan has no poll failure question: %#v", questions)
	}
	projected, _, versionIDs, err := db.WorkflowInboxProjection(ctx, repository)
	if err != nil || len(versionIDs) != 1 || versionIDs[0] != versionID || len(projected) == 0 {
		t.Fatalf("projecting recovery Inbox = versions %v, questions %#v, %v", versionIDs, projected, err)
	}
	var recoveryQuestion store.WorkflowQuestion
	for _, question := range questions {
		if question.VersionID == versionID && question.Kind == "poll_failure" {
			recoveryQuestion = question
		}
	}
	leaseToken, ok := poller.pollLeaseToken(leaseCtx, repository)
	if !ok {
		t.Fatal("poll lease token missing")
	}
	outbox, err := db.AnswerWorkflowQuestionAndQueueInboxProjectionLeased(leaseCtx, repository, recoveryQuestion.ID, "retry", now.Add(time.Second), leaseToken, now)
	if err != nil {
		t.Fatalf("answer projecting recovery question = %v", err)
	}
	if outbox.Request.InboxProjectionGeneration == 0 || len(outbox.Request.WorkflowQuestions) != 0 || len(outbox.Request.InboxPlanVersionIDs) != 0 {
		t.Fatalf("answered recovery Inbox projection = %#v, want authoritative empty generation", outbox.Request)
	}
	cursor, err = db.GitHubPollCursor(ctx, repository)
	if err != nil || cursor.NeedsAttention() || cursor.ConsecutiveFailures != 0 {
		t.Fatalf("answered recovery cursor = %#v, %v", cursor, err)
	}
}

func TestPausedGatewayHTTPFailurePausesPollRecovery(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := "owner/repo"
	activatePollerPlan(t, ctx, db, repository)
	now := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)
	paused := &delivery.HTTPError{StatusCode: http.StatusServiceUnavailable, Code: delivery.ErrorCodeGatewayWritesPaused, Message: "writes paused"}
	_, err = (Poller{
		Store: db, Client: NewClient("http://example.invalid", "", nil).WithRepositoryOwner("owner"),
		InboxProjector: errorInboxProjector{err: paused}, Now: func() time.Time { return now },
	}).Poll(ctx, repository)
	if !errors.Is(err, paused) || errors.Is(err, store.ErrNeedsAttention) {
		t.Fatalf("paused Gateway poll = %v", err)
	}
	writesPaused, _, pauseErr := db.GatewayWritesPaused(ctx)
	cursor, cursorErr := db.GitHubPollCursor(ctx, repository)
	if pauseErr != nil || !writesPaused || cursorErr != nil || cursor.ConsecutiveFailures != 0 || cursor.NeedsAttention() {
		t.Fatalf("paused Gateway state = paused %t/%v cursor %#v/%v", writesPaused, pauseErr, cursor, cursorErr)
	}
}

func TestMidPollCredentialFailurePausesGatewayWithoutTerminalizingPlan(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := "owner/repo"
	activatePollerPlan(t, ctx, db, repository)
	now := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)
	if err := db.RecordGitHubPollFailureWithKind(ctx, repository, now.Add(-time.Minute), store.GitHubPollFailurePreActivationInboxConflict); err != nil {
		t.Fatal(err)
	}
	credentialErr := &APIError{StatusCode: http.StatusUnauthorized, Message: "bad credentials"}
	_, err = (Poller{
		Store:          db,
		Client:         NewClient("http://example.invalid", "", nil).WithRepositoryOwner("owner"),
		InboxProjector: errorInboxProjector{err: credentialErr},
		MaxFailures:    5,
		Now:            func() time.Time { return now },
	}).Poll(ctx, repository)
	if !errors.Is(err, credentialErr) || errors.Is(err, store.ErrNeedsAttention) {
		t.Fatalf("credential poll error = %v", err)
	}
	paused, _, pauseErr := db.GatewayWritesPaused(ctx)
	if pauseErr != nil || !paused {
		t.Fatalf("Gateway pause = %v, %v", paused, pauseErr)
	}
	active, activeErr := db.HasActiveDeliveryPlan(ctx, repository)
	if activeErr != nil || !active {
		t.Fatalf("active plan after credential failure = %v, %v", active, activeErr)
	}
	cursor, cursorErr := db.GitHubPollCursor(ctx, repository)
	if cursorErr != nil || cursor.NeedsAttention() || cursor.RecoveryState != store.GitHubPollRecoveryConsumed || cursor.ConsecutiveFailures != 0 {
		t.Fatalf("credential cursor = %#v, %v", cursor, cursorErr)
	}
	if _, err := (Poller{Store: db, MaxFailures: 2, Now: func() time.Time { return now.Add(time.Second) }}).RecordFailure(ctx, repository, errors.New("temporary GitHub failure")); errors.Is(err, store.ErrNeedsAttention) {
		t.Fatalf("first post-credential retry terminalized polling: %v", err)
	}
}

func TestExpiredPollLeaseCannotPersistFailure(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := "owner/repo"
	now := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)
	clock := now
	poller := Poller{Store: db, Now: func() time.Time { return clock }}
	staleCtx, staleRelease, err := poller.AcquireLease(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	clock = now.Add(githubPollLeaseTTL + time.Second)
	currentCtx, currentRelease, err := poller.AcquireLease(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := poller.RecordFailure(staleCtx, repository, errors.New("stale failure")); !errors.Is(err, store.ErrFencingConflict) {
		t.Fatalf("stale failure mutation = %v, want fencing conflict", err)
	}
	cursor, err := db.GitHubPollCursor(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	if cursor.ConsecutiveFailures != 0 {
		t.Fatalf("stale lease persisted %d failures", cursor.ConsecutiveFailures)
	}
	if _, err := poller.RecordFailure(currentCtx, repository, errors.New("current failure")); err == nil || errors.Is(err, store.ErrFencingConflict) {
		t.Fatalf("current lease failure mutation = %v", err)
	}
	if err := currentRelease(); err != nil {
		t.Fatal(err)
	}
	if err := staleRelease(); !errors.Is(err, store.ErrFencingConflict) {
		t.Fatalf("stale release = %v, want fencing conflict", err)
	}
}

func beginProjectingPollerPlan(t *testing.T, ctx context.Context, db *store.Store, repository string) string {
	t.Helper()
	return beginProjectingPollerPlanAt(t, ctx, db, repository, 1, 1, 2, 2)
}

func beginProjectingPollerPlanAt(t *testing.T, ctx context.Context, db *store.Store, repository string, rootID, rootNumber, ticketID, ticketNumber int64) string {
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
	version, err := db.BeginActivation(ctx, snapshot, fingerprint, "poller")
	if err != nil {
		t.Fatal(err)
	}
	return version.ID
}

func TestRecordFailurePersistsAfterParentCancellation(t *testing.T) {
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithCancel(context.Background())
	poller := Poller{Store: db, Now: func() time.Time { return time.Now().UTC() }}
	leaseCtx, release, err := poller.AcquireLease(ctx, "owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	now := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	_, err = poller.recordFailure(leaseCtx, "owner/repo", now, context.DeadlineExceeded)
	err = errors.Join(err, release())
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
	if !cursor.NextAttemptAt.Equal(retryAt) || cursor.ConsecutiveFailures != 1 || cursor.FailureKind != store.GitHubPollFailureRetryable || cursor.RecoveryState != store.GitHubPollRecoveryConsumed {
		t.Fatalf("cursor = %#v", cursor)
	}
}

func TestRecordFailureBoundsRepeatedRateLimits(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := "owner/repo"
	activatePollerPlan(t, ctx, db, repository)
	now := time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC)
	poller := Poller{Store: db, MaxFailures: 2, Now: func() time.Time { return now }}
	first := &apiError{StatusCode: 403, RetryAt: now.Add(time.Minute)}
	if _, err := poller.RecordFailure(ctx, repository, first); !errors.Is(err, first) || errors.Is(err, store.ErrNeedsAttention) {
		t.Fatalf("first rate limit = %v", err)
	}
	now = first.RetryAt
	second := &apiError{StatusCode: 403, RetryAt: now.Add(time.Minute)}
	if _, err := poller.RecordFailure(ctx, repository, second); !errors.Is(err, second) || !errors.Is(err, store.ErrNeedsAttention) {
		t.Fatalf("exhausted rate limit = %v", err)
	}
	cursor, err := db.GitHubPollCursor(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	if !cursor.NeedsAttention() || cursor.ConsecutiveFailures != 2 {
		t.Fatalf("exhausted rate-limit cursor = %#v", cursor)
	}
}

func TestRateLimitConsumesExhaustedBootstrapRecovery(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := "owner/repo"
	now := time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC)
	if err := db.RecordGitHubPollFailureWithKind(ctx, repository, now, store.GitHubPollFailurePreActivationInboxConflict); err != nil {
		t.Fatal(err)
	}
	retryAt := now.Add(time.Minute)
	readyAt := now.Add(time.Second)
	_, err = (Poller{Store: db, MaxFailures: 1, Now: func() time.Time { return readyAt }}).RecordFailure(ctx, repository, &apiError{StatusCode: 403, RetryAt: retryAt})
	if !errors.Is(err, store.ErrNeedsAttention) {
		t.Fatalf("rate-limited exhausted poll = %v, want needs attention", err)
	}
	cursor, err := db.GitHubPollCursor(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	if cursor.FailureKind != store.GitHubPollFailureUnrecoverable || cursor.RecoveryState != store.GitHubPollRecoveryConsumed {
		t.Fatalf("cursor provenance = %q/%q, want unrecoverable/consumed", cursor.FailureKind, cursor.RecoveryState)
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

func TestPollHonorsNextAttemptForNeedsAttentionCursor(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := "owner/repo"
	activatePollerPlan(t, ctx, db, repository)
	now := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)
	if err := db.DeferGitHubPoll(ctx, repository, now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkGitHubPollFailureUnrecoverable(ctx, repository, now); err != nil {
		t.Fatal(err)
	}
	projector := &countingInboxProjector{}
	_, err = (Poller{
		Store:          db,
		Client:         NewClient("http://example.invalid", "", nil).WithRepositoryOwner("owner"),
		InboxProjector: projector,
		Now:            func() time.Time { return now },
	}).Poll(ctx, repository)
	if !errors.Is(err, store.ErrNotReady) || projector.calls != 0 {
		t.Fatalf("paused deferred poll error=%v projections=%d", err, projector.calls)
	}
}
