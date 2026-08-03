package scheduler_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skyhuang233/workflow/internal/plan"
	"github.com/skyhuang233/workflow/internal/scheduler"
	"github.com/skyhuang233/workflow/internal/store"
)

type reader struct{ snapshot plan.Snapshot }

func (r *reader) ReadPlan(context.Context, string, int64) (plan.Snapshot, error) {
	return r.snapshot, nil
}

type projector struct{ body string }

func (p *projector) ProjectPlan(_ context.Context, _ string, _ int64, projection plan.Projection, _ string) error {
	body, err := plan.RenderProjection(p.body, projection)
	if err == nil {
		p.body = body
	}
	return err
}

func TestDispatcherClaimsAndProjectsRunningTicket(t *testing.T) {
	ctx := context.Background()
	snapshot := plan.Snapshot{
		Repository: "owner/repo",
		Root:       plan.Issue{ID: 100, Number: 10, Body: "human specification", Labels: []string{plan.PlanLabel}},
		Children: []plan.Issue{
			{ID: 1, Number: 11, Title: "first", Labels: []string{plan.TicketLabel}, State: "open"},
			{ID: 2, Number: 12, Title: "blocked", Labels: []string{plan.TicketLabel}, State: "open"},
		},
		BlockedBy: map[int64][]plan.Issue{2: {{ID: 1, Number: 11, Labels: []string{plan.TicketLabel}, State: "open"}}},
	}
	dbPath := filepath.Join(t.TempDir(), "workflow.db")
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	fingerprint, err := snapshot.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	version, err := db.BeginActivation(ctx, snapshot, fingerprint, "source-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkActive(ctx, version.ID); err != nil {
		t.Fatal(err)
	}

	rootReader := &reader{snapshot: snapshot}
	rootProjector := &projector{body: snapshot.Root.Body + "\n\nhuman edit after snapshot"}
	claim, err := (scheduler.Dispatcher{
		Store:           db,
		Reader:          rootReader,
		Projector:       rootProjector,
		MaxParallelRuns: 1,
		LeaseTTL:        time.Minute,
		Now:             func() time.Time { return time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC) },
	}).Claim(ctx, snapshot.Repository, snapshot.Root.Number, 1, "agent-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"human specification", "human edit after snapshot", "Running", "agent-1", claim.SessionID, claim.RunID} {
		if !strings.Contains(rootProjector.body, expected) {
			t.Fatalf("projected body %q does not contain %q", rootProjector.body, expected)
		}
	}

	frontier, err := db.ReadyFrontier(ctx, version.ID, 1, time.Date(2026, 7, 31, 0, 0, 1, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(frontier) != 0 {
		t.Fatalf("frontier after first claim = %#v, want empty at capacity", frontier)
	}

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	rootReader.snapshot.Root.Body = rootProjector.body
	recoveredProjector := &projector{}
	if err := (scheduler.Dispatcher{Store: restarted, Reader: rootReader, Projector: recoveredProjector, Now: func() time.Time {
		return time.Date(2026, 7, 31, 0, 0, 1, 0, time.UTC)
	}}).Recover(ctx, snapshot.Repository, snapshot.Root.Number); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(recoveredProjector.body, claim.RunID) || !strings.Contains(recoveredProjector.body, "agent-1") {
		t.Fatalf("recovered projection = %q", recoveredProjector.body)
	}
	recovered, err := restarted.CurrentClaim(ctx, version.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.RunID != claim.RunID || recovered.SessionID != claim.SessionID {
		t.Fatalf("recovered claim = %#v, want run %q/session %q", recovered, claim.RunID, claim.SessionID)
	}
}
