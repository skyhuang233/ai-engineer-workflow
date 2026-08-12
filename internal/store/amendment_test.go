package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skyhuang233/workflow/internal/plan"
)

func TestPlanAmendmentIsolatesAffectedAgentBeforeRevokingLease(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	snapshot := amendmentSnapshot(10, []plan.Issue{
		{ID: 1, Number: 11, Title: "first", Labels: []string{plan.TicketLabel}},
		{ID: 2, Number: 12, Title: "second", Labels: []string{plan.TicketLabel}},
	}, map[int64][]plan.Issue{2: {{ID: 1, Number: 11, Labels: []string{plan.TicketLabel}}}})
	version := activateAmendmentSnapshot(t, ctx, db, snapshot)
	claim, err := db.ClaimReady(ctx, ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent-1", MaxParallelRuns: 2, LeaseTTL: time.Hour, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ReserveWorkerLaunch(ctx, claim, WorkerAudit{RunID: claim.RunID, LeaseGeneration: claim.LeaseGeneration, ImageDigest: "sha256:image", ToolVersions: map[string]string{"codex": "1", "github-cli": "1", "go": "1", "no-mistakes": "1"}}, now); err != nil {
		t.Fatal(err)
	}
	amendment := PlanAmendment{
		VersionID:          version.ID,
		TicketID:           1,
		Summary:            "ticket #11 discovered that the dependency is obsolete",
		RemoveDependencies: []AmendmentEdge{{BlockedTicketID: 2, BlockerTicketID: 1}},
	}
	_, err = db.ProposePlanAmendment(ctx, amendment, now.Add(time.Minute))
	var isolation *WorkerIsolationRequired
	if !errors.As(err, &isolation) || len(isolation.Targets) != 1 || isolation.Targets[0].RunID != claim.RunID {
		t.Fatalf("agent isolation requirement = %#v, %v", isolation, err)
	}
	fenced, err := db.FenceWorkerIsolation(ctx, isolation.Targets)
	if err != nil {
		t.Fatal(err)
	}
	proofs, err := db.AcknowledgeWorkerIsolation(ctx, fenced)
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := db.ProposePlanAmendment(ctx, amendment, now.Add(time.Minute), proofs...)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AnswerWorkflowQuestion(ctx, snapshot.Repository, proposal.QuestionID, `{"action":"reject"}`, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	replacement, err := db.ClaimReady(ctx, ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent-2", MaxParallelRuns: 2, LeaseTTL: time.Hour, Now: now.Add(3 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if replacement.RunID == claim.RunID {
		t.Fatalf("replacement retained isolated Agent run %q", claim.RunID)
	}
}

func TestPlanAmendmentPausesOnlyAffectedSubgraphAndAppliesOneApprovedVersion(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	primary := amendmentSnapshot(10, []plan.Issue{
		{ID: 1, Number: 11, Title: "first", Labels: []string{plan.TicketLabel}},
		{ID: 2, Number: 12, Title: "second", Labels: []string{plan.TicketLabel}},
	}, map[int64][]plan.Issue{2: {{ID: 1, Number: 11, Labels: []string{plan.TicketLabel}}}})
	primaryVersion := activateAmendmentSnapshot(t, ctx, db, primary)
	secondary := amendmentSnapshot(20, []plan.Issue{{ID: 3, Number: 21, Title: "unaffected", Labels: []string{plan.TicketLabel}}}, nil)
	secondaryVersion := activateAmendmentSnapshot(t, ctx, db, secondary)

	proposal, err := db.ProposePlanAmendment(ctx, PlanAmendment{
		VersionID:          primaryVersion.ID,
		TicketID:           1,
		Summary:            "ticket #11 discovered that the dependency is obsolete",
		RemoveDependencies: []AmendmentEdge{{BlockedTicketID: 2, BlockerTicketID: 1}},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"Nodes added: none", "Edges removed: #12 blocked by #11", "Affected tickets: #11, #12", "Worker Runs/PRs: none", "Candidate revisions: none", "Cross-plan dependents: none"} {
		if !strings.Contains(proposal.Impact, expected) {
			t.Fatalf("impact report = %q, want %q", proposal.Impact, expected)
		}
	}

	frontier, err := db.GlobalReadyFrontier(ctx, primary.Repository, 1, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(frontier) != 1 || frontier[0].VersionID != secondaryVersion.ID || frontier[0].Ticket.IssueID != 3 {
		t.Fatalf("pending amendment frontier = %#v, want only unaffected ticket", frontier)
	}

	if err := db.AnswerWorkflowQuestion(ctx, primary.Repository, proposal.QuestionID, `{"action":"reject"}`, now); err != nil {
		t.Fatal(err)
	}
	if err := db.AnswerWorkflowQuestion(ctx, primary.Repository, proposal.QuestionID, `{"action":"reject"}`, now); err != nil {
		t.Fatalf("repeated rejection = %v", err)
	}
	frontier, err = db.GlobalReadyFrontier(ctx, primary.Repository, 2, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(frontier) != 2 || frontier[0].VersionID != primaryVersion.ID || frontier[0].Ticket.IssueID != 1 {
		t.Fatalf("rejected amendment frontier = %#v, want restored primary ticket then unaffected ticket", frontier)
	}

	proposal, err = db.ProposePlanAmendment(ctx, PlanAmendment{
		VersionID:          primaryVersion.ID,
		TicketID:           1,
		Summary:            "ticket #11 discovered that the dependency is obsolete",
		RemoveDependencies: []AmendmentEdge{{BlockedTicketID: 2, BlockerTicketID: 1}},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AnswerWorkflowQuestion(ctx, primary.Repository, proposal.QuestionID, `{"action":"approve"}`, now); err != nil {
		t.Fatal(err)
	}
	if err := db.AnswerWorkflowQuestion(ctx, primary.Repository, proposal.QuestionID, `{"action":"approve"}`, now); err != nil {
		t.Fatalf("repeated approval = %v", err)
	}
	updated, err := db.CurrentVersion(ctx, primary.Repository, primary.Root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID == primaryVersion.ID {
		t.Fatal("approved amendment retained the retired plan version")
	}
	updatedFrontier, err := db.ReadyFrontier(ctx, updated.ID, 2, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(updatedFrontier) != 2 || updatedFrontier[0].IssueID != 1 || updatedFrontier[1].IssueID != 2 {
		t.Fatalf("approved amendment frontier = %#v, want both unblocked tickets", updatedFrontier)
	}
}

func amendmentSnapshot(rootID int64, tickets []plan.Issue, dependencies map[int64][]plan.Issue) plan.Snapshot {
	return plan.Snapshot{
		Repository: "owner/repo",
		Root:       plan.Issue{ID: rootID, Number: rootID, Title: "plan", Labels: []string{plan.PlanLabel}},
		Children:   tickets,
		BlockedBy:  dependencies,
	}
}

func activateAmendmentSnapshot(t *testing.T, ctx context.Context, db *Store, snapshot plan.Snapshot) PlanVersion {
	t.Helper()
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
	return version
}
