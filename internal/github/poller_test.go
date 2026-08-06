package github

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

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
