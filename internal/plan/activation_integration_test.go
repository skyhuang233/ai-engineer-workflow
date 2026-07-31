package plan_test

import (
	"context"
	"path/filepath"
	"testing"

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

func (p *integrationProjector) UpdateIssueBody(_ context.Context, _ string, _ int64, body string) error {
	p.body = body
	return nil
}

func (p *integrationProjector) AddIssueLabel(_ context.Context, _ string, _ int64, label string) error {
	p.label = label
	return nil
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
	projector := &integrationProjector{}
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
