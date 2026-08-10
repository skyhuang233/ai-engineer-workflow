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

type rootProjector struct{ projected map[int64]plan.Projection }

func (p *rootProjector) ProjectPlan(_ context.Context, _ string, root int64, projection plan.Projection, _ string) error {
	p.projected[root] = projection
	return nil
}

type recoveryInspector struct {
	containerRunning bool
	workspaceReady   bool
	isolate          func(string)
}

type pressureInspector struct{ reason string }

func (p pressureInspector) Inspect(context.Context) (string, error) {
	return p.reason, nil
}

func (r recoveryInspector) ContainerRunning(context.Context, string) (bool, error) {
	return r.containerRunning, nil
}

func (r recoveryInspector) IsolateContainer(_ context.Context, runID string) error {
	if r.isolate != nil {
		r.isolate(runID)
	}
	return nil
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
		ProvisionSession: func(_ context.Context, provisioning store.SessionProvisioning) (store.SessionProvisioningResult, error) {
			if provisioning.SessionID == "" || provisioning.Existing {
				t.Fatalf("provisioning target = %q existing=%t, want a new Session", provisioning.SessionID, provisioning.Existing)
			}
			return store.SessionProvisioningResult{}, admissionErr
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
	t.Logf("failed ChatGPT admission consumed no Worker attempt; the first durable claim remained attempt %d", claim.Attempt)
}

func TestClaimNextRoundRobinsPlansAndProjectsOnlyTheirOwningRoots(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, snapshot := range []plan.Snapshot{
		{Repository: "owner/repo", Root: plan.Issue{ID: 100, Number: 10, Body: "plan A", Labels: []string{plan.PlanLabel}}, Children: []plan.Issue{{ID: 1, Number: 11, Title: "A", Labels: []string{plan.TicketLabel}, State: "open"}}},
		{Repository: "owner/repo", Root: plan.Issue{ID: 200, Number: 20, Body: "plan B", Labels: []string{plan.PlanLabel}}, Children: []plan.Issue{{ID: 2, Number: 21, Title: "B", Labels: []string{plan.TicketLabel}, State: "open"}}},
	} {
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
	}
	projector := &rootProjector{projected: make(map[int64]plan.Projection)}
	dispatcher := scheduler.Dispatcher{Store: db, Projector: projector, MaxParallelRuns: 2, LeaseTTL: time.Hour}
	first, err := dispatcher.ClaimNext(ctx, "owner/repo", "agent")
	if err != nil {
		t.Fatal(err)
	}
	second, err := dispatcher.ClaimNext(ctx, "owner/repo", "agent")
	if err != nil {
		t.Fatal(err)
	}
	if first.PlanRootNumber != 10 || second.PlanRootNumber != 20 || len(projector.projected) != 2 {
		t.Fatalf("global claims=%d,%d projected=%#v", first.PlanRootNumber, second.PlanRootNumber, projector.projected)
	}
}

func TestHostPressurePausesNewDispatchWithoutChangingActiveRuns(t *testing.T) {
	ctx := context.Background()
	snapshot := plan.Snapshot{Repository: "owner/repo", Root: plan.Issue{ID: 100, Number: 10, Body: "spec", Labels: []string{plan.PlanLabel}}, Children: []plan.Issue{{ID: 1, Number: 11, Title: "running", Labels: []string{plan.TicketLabel}, State: "open"}, {ID: 2, Number: 12, Title: "queued", Labels: []string{plan.TicketLabel}, State: "open"}}}
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
	// CurrentClaim evaluates lease liveness against the real clock, so anchor
	// this active-run invariant to the test's current time rather than a date
	// that eventually becomes an expired lease.
	now := time.Now().UTC().Truncate(time.Second)
	running, err := db.ClaimReady(ctx, store.ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent", MaxParallelRuns: 2, LeaseTTL: time.Hour, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	root := &projector{body: snapshot.Root.Body}
	dispatcher := scheduler.Dispatcher{Store: db, Projector: root, MaxParallelRuns: 2, LeaseTTL: time.Hour, HostPressure: pressureInspector{reason: "Docker health check failed"}, Now: func() time.Time { return now }}
	if _, err := dispatcher.ClaimNext(ctx, snapshot.Repository, "another-agent"); !errors.Is(err, store.ErrCapacity) {
		t.Fatalf("pressure claim error = %v, want ErrCapacity", err)
	}
	current, err := db.CurrentClaim(ctx, version.ID, running.TicketID)
	if err != nil || current.RunID != running.RunID {
		t.Fatalf("pressure changed active claim = %#v, %v", current, err)
	}
	if !strings.Contains(root.body, "new dispatches: paused") || !strings.Contains(root.body, "Docker health check failed") {
		t.Fatalf("pressure projection = %q", root.body)
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

func TestRecoverIsolatesExpiredContainerBeforeReplacementRun(t *testing.T) {
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
	claimedAt := time.Date(2099, 8, 9, 12, 0, 0, 0, time.UTC)
	now := claimedAt.Add(2 * time.Minute)
	expired, err := db.ClaimReady(ctx, store.ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent-1", MaxParallelRuns: 1, LeaseTTL: time.Minute, Now: claimedAt})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ReserveWorkerLaunch(ctx, expired, store.WorkerAudit{RunID: expired.RunID, LeaseGeneration: expired.LeaseGeneration, ImageDigest: "sha256:old", ToolVersions: map[string]string{"codex": "1", "github-cli": "1", "go": "1", "no-mistakes": "1"}}, claimedAt); err != nil {
		t.Fatal(err)
	}
	var isolated []string
	dispatcher := scheduler.Dispatcher{Store: db, Reader: &reader{snapshot: snapshot}, Projector: &projector{}, Recovery: recoveryInspector{containerRunning: true, workspaceReady: true, isolate: func(runID string) { isolated = append(isolated, runID) }}, Now: func() time.Time { return now }}
	if err := dispatcher.Recover(ctx, snapshot.Repository, snapshot.Root.Number); err != nil {
		t.Fatal(err)
	}
	if len(isolated) != 1 || isolated[0] != expired.RunID {
		t.Fatalf("isolated runs = %#v, want [%q]", isolated, expired.RunID)
	}
	if err := db.AcceptCandidate(ctx, store.CandidateRevision{RunID: expired.RunID, LeaseToken: expired.LeaseToken, CodexSessionID: "codex-session", CommitSHA: "stale", StructuredOutput: []byte(`{"summary":"stale","checks":[{"command":"go test","outcome":"passed"}]}`), Now: now, Publication: store.CandidatePublication{Repository: snapshot.Repository, Branch: "ticket-1", ExpectRemoteAbsent: true, Title: "ticket"}}); !errors.Is(err, store.ErrInvalidClaim) {
		t.Fatalf("late Candidate acceptance = %v, want ErrInvalidClaim", err)
	}
	replacement, err := db.ClaimReady(ctx, store.ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent-2", MaxParallelRuns: 1, LeaseTTL: time.Hour, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if replacement.SessionID != expired.SessionID || replacement.LeaseGeneration != expired.LeaseGeneration+1 {
		t.Fatalf("replacement = %#v, want retained Session and next Lease generation", replacement)
	}
	audit, err := db.WorkerAudit(ctx, expired.RunID)
	if err != nil || audit.ImageDigest != "sha256:old" {
		t.Fatalf("old Run audit = %#v, %v", audit, err)
	}
}

func TestRecoverFailsRunWhenEstablishedSessionAuthenticationIsUnavailable(t *testing.T) {
	ctx := context.Background()
	snapshot := plan.Snapshot{Repository: "owner/repo", Root: plan.Issue{ID: 100, Number: 10, Body: "spec", Labels: []string{plan.PlanLabel}}, Children: []plan.Issue{{ID: 1, Number: 11, Title: "first", Labels: []string{plan.TicketLabel}, State: "open"}}}
	dbPath := filepath.Join(t.TempDir(), "workflow.db")
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
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
	provisionCalls := 0
	dispatcher := scheduler.Dispatcher{
		Store: db, Reader: &reader{snapshot: snapshot}, Projector: &projector{},
		Recovery: recoveryInspector{workspaceReady: true}, Now: func() time.Time { return now.Add(time.Minute) },
		ProvisionSession: func(_ context.Context, provisioning store.SessionProvisioning) (store.SessionProvisioningResult, error) {
			provisionCalls++
			if !provisioning.Existing || provisioning.CurrentRunID != claim.RunID {
				t.Fatalf("recovery provisioning = %#v", provisioning)
			}
			return store.SessionProvisioningResult{}, &store.SessionAuthenticationFailure{}
		},
	}
	if err := dispatcher.Recover(ctx, snapshot.Repository, snapshot.Root.Number); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CurrentClaim(ctx, version.ID, claim.TicketID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("authentication-corrupt recovery Run remained active: %v", err)
	}
	projection, err := db.PlanProjection(ctx, version.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.Tickets) != 1 || projection.Tickets[0].State != "Needs Attention" {
		t.Fatalf("authentication-corrupt recovery projection = %#v", projection.Tickets)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	db = restarted
	dispatcher.Store = restarted
	dispatcher.Projector = &projector{}
	if err := dispatcher.Recover(ctx, snapshot.Repository, snapshot.Root.Number); err != nil {
		t.Fatal(err)
	}
	if provisionCalls != 1 {
		t.Fatalf("authentication provisioning repeated after restart: calls = %d, want 1", provisionCalls)
	}
	projection, err = restarted.PlanProjection(ctx, version.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.Tickets) != 1 || projection.Tickets[0].State != "Needs Attention" {
		t.Fatalf("authentication-corrupt projection after restart = %#v", projection.Tickets)
	}
	if _, err := restarted.CurrentClaim(ctx, version.ID, claim.TicketID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("authentication-corrupt Run became current after restart: %v", err)
	}
	t.Logf("after reopening SQLite and rerunning recovery: state=%s current_claim=false auth_provision_calls=%d", projection.Tickets[0].State, provisionCalls)
}
