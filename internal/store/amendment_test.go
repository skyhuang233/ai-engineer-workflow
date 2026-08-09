package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skyhuang233/workflow/internal/plan"
)

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
