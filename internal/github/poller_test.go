package github

import (
	"context"
	"errors"
	"net/http/httptest"
	"path/filepath"
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

func (p *activePlanInboxProjector) ProjectWorkflowInbox(ctx context.Context, repository string, _ []plan.WorkflowQuestion) error {
	p.calls++
	_, err := p.store.EnqueueDelivery(ctx, store.DeliveryRequest{
		Operation:  store.DeliveryProjectInbox,
		Repository: repository,
	}, p.now)
	return err
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
	server := httptest.NewServer(delivery.HTTPHandler(delivery.Gateway{Store: db}, delivery.HTTPOptions{ControlPlaneToken: controlToken}))
	defer server.Close()
	_, err = (Poller{
		Store:          db,
		Client:         NewClient("http://example.invalid", "", nil).WithRepositoryOwner("owner"),
		InboxProjector: delivery.HTTPProjector{URL: server.URL, ControlPlaneToken: controlToken},
		MaxFailures:    1,
		Now:            func() time.Time { return failedAt },
	}).Poll(ctx, repository)
	if err == nil {
		t.Fatal("cold-start inbox conflict returned nil")
	}
	cursor, err := db.GitHubPollCursor(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	if cursor.FailureKind != store.GitHubPollFailurePreActivationInboxConflict {
		t.Fatalf("failure kind = %q", cursor.FailureKind)
	}
	projector := &activePlanInboxProjector{store: db, now: now}
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
	if bootstrapRuns != 1 || controlRuns != 1 || projector.calls != 1 {
		t.Fatalf("bootstrap, control, projection = %d, %d, %d", bootstrapRuns, controlRuns, projector.calls)
	}
	cursor, err = db.GitHubPollCursor(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	if cursor.ConsecutiveFailures != 0 {
		t.Fatalf("consecutive failures = %d, want 0", cursor.ConsecutiveFailures)
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
