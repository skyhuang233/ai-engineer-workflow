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
	now := time.Now().UTC()
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
	if err := db.ReserveWorkerPrelaunch(ctx, claim, now); err != nil {
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
	now := time.Now().UTC()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	primary := amendmentSnapshot(10, []plan.Issue{
		{ID: 1, Number: 11, Title: "first", Labels: []string{plan.TicketLabel}},
		{ID: 2, Number: 12, Title: "second", Labels: []string{plan.TicketLabel}},
		{ID: 3, Number: 13, Title: "unaffected", Labels: []string{plan.TicketLabel}},
		{ID: 5, Number: 15, Title: "unaffected gate", Labels: []string{plan.TicketLabel}},
		{ID: 6, Number: 16, Title: "unaffected queued agent", Labels: []string{plan.TicketLabel}},
	}, map[int64][]plan.Issue{2: {{ID: 1, Number: 11, Labels: []string{plan.TicketLabel}}}})
	primaryVersion := activateAmendmentSnapshot(t, ctx, db, primary)
	secondary := amendmentSnapshot(20, []plan.Issue{{ID: 4, Number: 21, Title: "other plan", Labels: []string{plan.TicketLabel}}}, nil)
	secondaryVersion := activateAmendmentSnapshot(t, ctx, db, secondary)
	unaffectedClaim, err := db.ClaimReady(ctx, ClaimRequest{VersionID: primaryVersion.ID, TicketID: 3, Owner: "agent-3", MaxParallelRuns: 4, LeaseTTL: time.Hour, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	unaffectedClaim, err = db.AcceptCandidateForDelivery(ctx, CandidateRevision{RunID: unaffectedClaim.RunID, LeaseToken: unaffectedClaim.LeaseToken, CodexSessionID: "codex-3", CommitSHA: "accepted-3", StructuredOutput: []byte(`{"summary":"candidate","checks":[{"command":"go test","outcome":"passed"}]}`), Now: now, Publication: CandidatePublication{Repository: primary.Repository, Branch: "ticket-3", ExpectRemoteAbsent: true, Title: "ticket-3"}}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	unaffectedGateClaim, err := db.ClaimReady(ctx, ClaimRequest{VersionID: primaryVersion.ID, TicketID: 5, Owner: "agent-5", MaxParallelRuns: 4, LeaseTTL: time.Hour, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	unaffectedGateClaim, err = db.AcceptCandidateForDelivery(ctx, CandidateRevision{RunID: unaffectedGateClaim.RunID, LeaseToken: unaffectedGateClaim.LeaseToken, CodexSessionID: "codex-5", CommitSHA: "accepted-5", StructuredOutput: []byte(`{"summary":"candidate","checks":[{"command":"go test","outcome":"passed"}]}`), Now: now, Publication: CandidatePublication{Repository: primary.Repository, Branch: "ticket-5", ExpectRemoteAbsent: true, Title: "ticket-5"}}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	unaffectedAgentClaim, err := db.ClaimReady(ctx, ClaimRequest{VersionID: primaryVersion.ID, TicketID: 6, Owner: "agent-6", MaxParallelRuns: 4, LeaseTTL: time.Hour, Now: now})
	if err != nil {
		t.Fatal(err)
	}

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

	frontier, err := db.GlobalReadyFrontier(ctx, primary.Repository, 4, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(frontier) != 1 || frontier[0].VersionID != secondaryVersion.ID || frontier[0].Ticket.IssueID != 4 {
		t.Fatalf("pending amendment frontier = %#v, want only unaffected ticket", frontier)
	}

	if err := db.AnswerWorkflowQuestion(ctx, primary.Repository, proposal.QuestionID, `{"action":"reject"}`, now); err != nil {
		t.Fatal(err)
	}
	if err := db.AnswerWorkflowQuestion(ctx, primary.Repository, proposal.QuestionID, `{"action":"reject"}`, now); err != nil {
		t.Fatalf("repeated rejection = %v", err)
	}
	frontier, err = db.GlobalReadyFrontier(ctx, primary.Repository, 5, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(frontier) != 2 || frontier[0].VersionID != primaryVersion.ID || frontier[0].Ticket.IssueID != 1 {
		t.Fatalf("rejected amendment frontier = %#v, want restored primary ticket then unaffected ticket", frontier)
	}
	if err := db.RequireCurrentDeliveryLease(ctx, unaffectedClaim, now); err != nil {
		t.Fatalf("unaffected delivery claim after rejection: %v", err)
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
	continued, err := db.TicketSession(ctx, updated.ID, 3)
	if err != nil || continued.SessionID != unaffectedClaim.SessionID {
		t.Fatalf("unaffected session after approval = %#v, %v", continued, err)
	}
	if _, err := db.TicketSession(ctx, primaryVersion.ID, 3); !errors.Is(err, ErrNotFound) {
		t.Fatalf("source version retained unaffected session: %v", err)
	}
	resolvedAgentClaim, agentSession, err := db.ResolveAgentLaunchContext(ctx, unaffectedAgentClaim, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("resolve queued Agent launch after approval: %v", err)
	}
	if resolvedAgentClaim.VersionID != updated.ID || agentSession.VersionID != updated.ID {
		t.Fatalf("queued Agent launch context versions = %q, %q, want %q", resolvedAgentClaim.VersionID, agentSession.VersionID, updated.ID)
	}
	resolvedDeliveryClaim, deliverySession, delivery, err := db.ResolveDeliveryLaunchContext(ctx, unaffectedClaim, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("resolve queued Delivery Controller launch after approval: %v", err)
	}
	if resolvedDeliveryClaim.VersionID != updated.ID || deliverySession.VersionID != updated.ID || delivery.VersionID != updated.ID {
		t.Fatalf("queued Delivery Controller launch context versions = %q, %q, %q, want %q", resolvedDeliveryClaim.VersionID, deliverySession.VersionID, delivery.VersionID, updated.ID)
	}
	if err := db.RequireCurrentDeliveryLease(ctx, unaffectedClaim, now.Add(time.Minute)); err != nil {
		t.Fatalf("source-version delivery claim lost after approval: %v", err)
	}
	if err := db.CompleteDeliveryController(ctx, unaffectedClaim, now.Add(time.Minute)); err != nil {
		t.Fatalf("source-version delivery completion after approval: %v", err)
	}
	gateQuestion, err := db.PauseDeliveryControllerForQualityGate(ctx, unaffectedGateClaim, QualityGate{GateID: "owner-decision", Action: QualityGateAskUser, Reason: "owner input required", AllowedAnswers: []string{"approve", "reject"}}, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("source-version quality gate after approval: %v", err)
	}
	if gateQuestion.VersionID != updated.ID {
		t.Fatalf("quality gate version = %q, want %q", gateQuestion.VersionID, updated.ID)
	}
	updatedFrontier, err := db.ReadyFrontier(ctx, updated.ID, 3, now)
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
