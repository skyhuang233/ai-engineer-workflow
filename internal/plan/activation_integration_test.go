package plan_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/skyhuang233/workflow/internal/plan"
	"github.com/skyhuang233/workflow/internal/store"
)

type integrationReader struct{ snapshot plan.Snapshot }

func (r *integrationReader) ReadPlan(context.Context, string, int64) (plan.Snapshot, error) {
	return r.snapshot, nil
}

type integrationProjector struct {
	body  string
	label string
}

func (p *integrationProjector) ProjectPlan(_ context.Context, _ string, _ int64, projection plan.Projection, label string) error {
	body, err := plan.RenderProjection(p.body, projection)
	if err == nil {
		p.body = body
	}
	if label != "" {
		p.label = label
	}
	return err
}

func TestActivationPathPersistsOneVersionAcrossRestart(t *testing.T) {
	ctx := context.Background()
	snapshot := plan.Snapshot{
		Repository: "owner/repo",
		Root:       plan.Issue{ID: 100, Number: 10, Body: "approved spec", Labels: []string{plan.PlanLabel}, UpdatedAt: "source-1"},
		Children: []plan.Issue{
			{ID: 1, Number: 11, Title: "first", Labels: []string{plan.TicketLabel}, State: "open"},
			{ID: 2, Number: 12, Title: "second", Labels: []string{plan.TicketLabel}, State: "open"},
		},
		BlockedBy: map[int64][]plan.Issue{2: {{ID: 1, Number: 11, Title: "first", Labels: []string{plan.TicketLabel}, State: "open"}}},
	}
	dbPath := filepath.Join(t.TempDir(), "workflow.db")
	runtimeStore, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	projector := &integrationProjector{body: snapshot.Root.Body}
	reader := &integrationReader{snapshot: snapshot}
	activator := plan.Activator{Reader: reader, Projector: projector, Store: runtimeStore}
	first, err := activator.Activate(ctx, snapshot.Repository, snapshot.Root.Number)
	if err != nil {
		t.Fatal(err)
	}
	reader.snapshot.Root.Body = projector.body
	reader.snapshot.Root.Labels = append(reader.snapshot.Root.Labels, plan.ActiveLabel)
	second, err := activator.Activate(ctx, snapshot.Repository, snapshot.Root.Number)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || first.State != "active" || second.State != "active" {
		t.Fatalf("activations = %#v, %#v", first, second)
	}
	if projector.label != plan.ActiveLabel {
		t.Fatalf("projected label = %q", projector.label)
	}
	if err := runtimeStore.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	recovered, err := restarted.CurrentVersion(ctx, snapshot.Repository, snapshot.Root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.ID != first.ID || recovered.State != store.StateActive {
		t.Fatalf("recovered = %#v", recovered)
	}
}

func TestActivationPreservesCompletedProjection(t *testing.T) {
	ctx := context.Background()
	snapshot := plan.Snapshot{
		Repository: "owner/repo",
		Root:       plan.Issue{ID: 100, Number: 10, Body: "approved spec", Labels: []string{plan.PlanLabel}, UpdatedAt: "source-1"},
		Children: []plan.Issue{
			{ID: 1, Number: 11, Title: "first", Labels: []string{plan.TicketLabel}, State: "open"},
		},
		BlockedBy: map[int64][]plan.Issue{},
	}
	runtimeStore, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer runtimeStore.Close()
	projector := &integrationProjector{body: snapshot.Root.Body}
	reader := &integrationReader{snapshot: snapshot}
	activator := plan.Activator{Reader: reader, Projector: projector, Store: runtimeStore}
	version, err := activator.Activate(ctx, snapshot.Repository, snapshot.Root.Number)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtimeStore.MarkTicketDelivered(ctx, version.ID, 1); err != nil {
		t.Fatal(err)
	}
	reader.snapshot.Root.Body = projector.body
	completed, err := activator.Activate(ctx, snapshot.Repository, snapshot.Root.Number)
	if err != nil {
		t.Fatal(err)
	}
	if completed.State != store.StateCompleted || !contains(projector.body, "`Completed`") {
		t.Fatalf("completed activation = %#v, body = %q", completed, projector.body)
	}
	t.Logf("Plan state=%s after its delivered Ticket was projected; projection contains Completed=%t", completed.State, contains(projector.body, "`Completed`"))
}

func TestActivationCompletesAnInitiallyDeliveredPlan(t *testing.T) {
	ctx := context.Background()
	snapshot := plan.Snapshot{
		Repository: "owner/repo",
		Root:       plan.Issue{ID: 100, Number: 10, Body: "approved spec", Labels: []string{plan.PlanLabel}, UpdatedAt: "source-1"},
		Children: []plan.Issue{
			{ID: 1, Number: 11, Title: "first", Labels: []string{plan.TicketLabel}, State: "closed", Delivered: true},
		},
		BlockedBy: map[int64][]plan.Issue{},
	}
	runtimeStore, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer runtimeStore.Close()
	projector := &integrationProjector{body: snapshot.Root.Body}
	version, err := (plan.Activator{Reader: &integrationReader{snapshot: snapshot}, Projector: projector, Store: runtimeStore}).Activate(ctx, snapshot.Repository, snapshot.Root.Number)
	if err != nil {
		t.Fatal(err)
	}
	if version.State != store.StateCompleted || !contains(projector.body, "`Completed`") {
		t.Fatalf("activation = %#v, body = %q", version, projector.body)
	}
}

func TestActivationRestoresDeliveredBlockersFromStore(t *testing.T) {
	ctx := context.Background()
	snapshot := plan.Snapshot{
		Repository: "owner/repo",
		Root:       plan.Issue{ID: 100, Number: 10, Body: "approved spec", Labels: []string{plan.PlanLabel}, UpdatedAt: "source-1"},
		Children: []plan.Issue{
			{ID: 1, Number: 11, Title: "first", Labels: []string{plan.TicketLabel}, State: "open"},
			{ID: 2, Number: 12, Title: "second", Labels: []string{plan.TicketLabel}, State: "open"},
		},
		BlockedBy: map[int64][]plan.Issue{2: {{ID: 1, Number: 11, Title: "first", Labels: []string{plan.TicketLabel}, State: "open"}}},
	}
	runtimeStore, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer runtimeStore.Close()
	reader := &integrationReader{snapshot: snapshot}
	activator := plan.Activator{Reader: reader, Projector: &integrationProjector{body: snapshot.Root.Body}, Store: runtimeStore}
	version, err := activator.Activate(ctx, snapshot.Repository, snapshot.Root.Number)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtimeStore.MarkTicketDelivered(ctx, version.ID, 1); err != nil {
		t.Fatal(err)
	}
	reader.snapshot.Children[0].State = "closed"
	reader.snapshot.BlockedBy[2][0].State = "closed"
	if _, err := activator.Activate(ctx, snapshot.Repository, snapshot.Root.Number); err != nil {
		t.Fatalf("activation with closed delivered blocker = %v", err)
	}
	frontier, err := runtimeStore.ReadyFrontier(ctx, version.ID, 1, time.Now().UTC())
	if err != nil || len(frontier) != 1 || frontier[0].IssueID != 2 {
		t.Fatalf("frontier after restored delivery = %#v, %v", frontier, err)
	}
}

func contains(value, target string) bool {
	return len(target) == 0 || (len(value) >= len(target) && index(value, target) >= 0)
}

func index(value, target string) int {
	for i := 0; i+len(target) <= len(value); i++ {
		if value[i:i+len(target)] == target {
			return i
		}
	}
	return -1
}
