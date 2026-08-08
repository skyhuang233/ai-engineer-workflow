package scheduler_test

import (
	"context"
	"errors"
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

type recoveryInspector struct {
	containerRunning bool
	workspaceReady   bool
}

func (r recoveryInspector) ContainerRunning(context.Context, string) (bool, error) {
	return r.containerRunning, nil
}

func (r recoveryInspector) WorkspaceAvailable(context.Context, store.TicketSession) (bool, error) {
	return r.workspaceReady, nil
}

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
	now := time.Now().UTC()
	claim, err := (scheduler.Dispatcher{
		Store:           db,
		Reader:          rootReader,
		Projector:       rootProjector,
		MaxParallelRuns: 1,
		LeaseTTL:        time.Minute,
		Now:             func() time.Time { return now },
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
		return now.Add(time.Second)
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

func TestDispatcherAdmissionFailureDoesNotConsumeWorkerAttempt(t *testing.T) {
	ctx := context.Background()
	snapshot := plan.Snapshot{
		Repository: "owner/repo",
		Root:       plan.Issue{ID: 100, Number: 10, Body: "spec", Labels: []string{plan.PlanLabel}},
		Children:   []plan.Issue{{ID: 1, Number: 11, Title: "first", Labels: []string{plan.TicketLabel}, State: "open"}},
	}
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
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
	admissionErr := errors.New("ChatGPT authentication unavailable")
	dispatcher := scheduler.Dispatcher{
		Store: db, Reader: &reader{snapshot: snapshot}, Projector: &projector{body: snapshot.Root.Body},
		MaxParallelRuns: 1, LeaseTTL: time.Minute,
		ProvisionSession: func(_ context.Context, provisioning store.SessionProvisioning) error {
			if provisioning.SessionID == "" || provisioning.Existing {
				t.Fatalf("provisioning target = %q existing=%t, want a new Session", provisioning.SessionID, provisioning.Existing)
			}
			return admissionErr
		},
	}
	if _, err := dispatcher.Claim(ctx, snapshot.Repository, snapshot.Root.Number, 0, "agent-1"); !errors.Is(err, admissionErr) {
		t.Fatalf("Claim() error = %v, want admission failure", err)
	}
	claim, err := db.ClaimReady(ctx, store.ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent-1", MaxParallelRuns: 1, LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if claim.Attempt != 1 {
		t.Fatalf("first durable claim attempt = %d, want 1", claim.Attempt)
	}
}

func TestRecoverReleasesMissingAgentContainerBeforeProjection(t *testing.T) {
	ctx := context.Background()
	snapshot := plan.Snapshot{Repository: "owner/repo", Root: plan.Issue{ID: 100, Number: 10, Body: "spec", Labels: []string{plan.PlanLabel}}, Children: []plan.Issue{{ID: 1, Number: 11, Title: "first", Labels: []string{plan.TicketLabel}, State: "open"}}}
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
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
	now := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	claim, err := db.ClaimReady(ctx, store.ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent-1", MaxParallelRuns: 1, LeaseTTL: time.Hour, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	root := &projector{}
	if err := (scheduler.Dispatcher{Store: db, Reader: &reader{snapshot: snapshot}, Projector: root, Recovery: recoveryInspector{workspaceReady: true}, Now: func() time.Time { return now.Add(time.Minute) }}).Recover(ctx, snapshot.Repository, snapshot.Root.Number); err != nil {
		t.Fatal(err)
	}
	replacement, err := db.ClaimReady(ctx, store.ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent-2", MaxParallelRuns: 1, LeaseTTL: time.Hour, Now: now.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if replacement.RunID == claim.RunID {
		t.Fatalf("recovery retained dead run %q", claim.RunID)
	}
}
