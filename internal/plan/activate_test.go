package plan

import (
	"context"
	"errors"
	"testing"
)

type fakeReader struct {
	snapshot Snapshot
}

func (f fakeReader) ReadPlan(context.Context, string, int64) (Snapshot, error) {
	return f.snapshot, nil
}

type fakeProjector struct {
	body          string
	bodies        []string
	label         string
	err           error
	beforeProject func(*fakeProjector)
}

func (f *fakeProjector) ProjectPlan(_ context.Context, _ string, _ int64, projection Projection, label string) error {
	if f.beforeProject != nil {
		f.beforeProject(f)
	}
	body, err := RenderProjection(f.body, projection)
	if err != nil {
		return err
	}
	f.body = body
	f.bodies = append(f.bodies, body)
	if label != "" {
		f.label = label
	}
	return errors.Join(err, f.err)
}

type fakeStore struct {
	version Version
	marked  string
}

func (f *fakeStore) BeginActivation(_ context.Context, _ Snapshot, fingerprint, source string) (Version, error) {
	if f.version.ID == "" {
		f.version = Version{ID: "pv-test", Fingerprint: fingerprint, SourceRevision: source, State: "projecting"}
	}
	return f.version, nil
}

func (f *fakeStore) MarkActive(_ context.Context, id string) error {
	f.marked = id
	f.version.State = "active"
	return nil
}

func (f *fakeStore) ActivationState(_ context.Context, _ string) (string, error) {
	return f.version.State, nil
}

func TestActivatorProjectsAndCommitsOnlyAfterProjection(t *testing.T) {
	reader := fakeReader{snapshot: Snapshot{
		Repository: "owner/repo",
		Root:       Issue{ID: 100, Number: 10, Body: "approved spec", Labels: []string{PlanLabel}},
		Children:   []Issue{{ID: 1, Number: 11, Title: "first", Labels: []string{TicketLabel}, State: "open"}},
		BlockedBy:  map[int64][]Issue{},
	}}
	projector := &fakeProjector{body: reader.snapshot.Root.Body}
	store := &fakeStore{}
	version, err := (Activator{Reader: reader, Projector: projector, Store: store}).Activate(context.Background(), "owner/repo", 10)
	if err != nil {
		t.Fatal(err)
	}
	if version.State != "active" || store.marked != "pv-test" {
		t.Fatalf("version = %#v, marked = %q", version, store.marked)
	}
	if projector.label != ActiveLabel {
		t.Fatalf("active label = %q, want %q", projector.label, ActiveLabel)
	}
	if projector.body == "" || !containsText(projector.body, "approved spec", ProjectionStart, "pv-test") {
		t.Fatalf("projection body = %q", projector.body)
	}
	if len(projector.bodies) != 2 || !containsText(projector.bodies[0], "`Building`") || !containsText(projector.bodies[1], "`Active`") {
		t.Fatalf("projection sequence = %#v", projector.bodies)
	}
}

func TestActivatorLeavesVersionProjectingWhenGitHubProjectionFails(t *testing.T) {
	reader := fakeReader{snapshot: Snapshot{
		Repository: "owner/repo",
		Root:       Issue{ID: 100, Number: 10, Labels: []string{PlanLabel}},
		Children:   []Issue{{ID: 1, Number: 11, Labels: []string{TicketLabel}, State: "open"}},
	}}
	projector := &fakeProjector{body: reader.snapshot.Root.Body, err: errors.New("timeout")}
	store := &fakeStore{}
	version, err := (Activator{Reader: reader, Projector: projector, Store: store}).Activate(context.Background(), "owner/repo", 10)
	if err == nil {
		t.Fatal("Activate() succeeded despite projection failure")
	}
	if version.ID != "pv-test" || version.State != "projecting" {
		t.Fatalf("Activate() version = %#v, want persisted projecting version", version)
	}
	if store.marked != "" || store.version.State != "projecting" {
		t.Fatalf("store = %#v, marked = %q; incomplete activation became active", store.version, store.marked)
	}
}

func TestActivatorPreservesHumanEditsMadeBetweenProjectionWrites(t *testing.T) {
	reader := fakeReader{snapshot: Snapshot{
		Repository: "owner/repo",
		Root:       Issue{ID: 100, Number: 10, Body: "approved spec", Labels: []string{PlanLabel}},
		Children:   []Issue{{ID: 1, Number: 11, Title: "first", Labels: []string{TicketLabel}, State: "open"}},
		BlockedBy:  map[int64][]Issue{},
	}}
	projector := &fakeProjector{body: reader.snapshot.Root.Body}
	projector.beforeProject = func(projector *fakeProjector) {
		if len(projector.bodies) == 1 {
			projector.body += "\n\nhuman edit during activation"
		}
	}
	if _, err := (Activator{Reader: reader, Projector: projector, Store: &fakeStore{}}).Activate(context.Background(), "owner/repo", 10); err != nil {
		t.Fatal(err)
	}
	if !containsString(projector.body, "human edit during activation") {
		t.Fatalf("active projection overwrote concurrent edit: %q", projector.body)
	}
}

func TestActivatorRejectsMalformedProjectionBeforePersistingVersion(t *testing.T) {
	reader := fakeReader{snapshot: Snapshot{
		Repository: "owner/repo",
		Root:       Issue{ID: 100, Number: 10, Body: ProjectionStart, Labels: []string{PlanLabel}},
		Children:   []Issue{{ID: 1, Number: 11, Labels: []string{TicketLabel}, State: "open"}},
	}}
	projector := &fakeProjector{}
	store := &fakeStore{}
	if _, err := (Activator{Reader: reader, Projector: projector, Store: store}).Activate(context.Background(), "owner/repo", 10); !errors.Is(err, ErrMalformedStatus) {
		t.Fatalf("Activate() error = %v, want ErrMalformedStatus", err)
	}
	if store.version.ID != "" {
		t.Fatalf("malformed projection persisted version %#v", store.version)
	}
}

func containsText(value string, expected ...string) bool {
	for _, item := range expected {
		if !containsString(value, item) {
			return false
		}
	}
	return true
}

func containsString(value, target string) bool {
	for i := 0; i+len(target) <= len(value); i++ {
		if value[i:i+len(target)] == target {
			return true
		}
	}
	return false
}
