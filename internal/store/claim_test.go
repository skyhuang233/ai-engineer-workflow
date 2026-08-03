package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/skyhuang233/workflow/internal/plan"
)

func TestClaimReadyCreatesSessionRunAndLeaseAtomically(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	snapshot := testSnapshot()
	fingerprint, err := snapshot.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	version, err := db.BeginActivation(ctx, snapshot, fingerprint, "revision-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkActive(ctx, version.ID); err != nil {
		t.Fatal(err)
	}

	claimed, err := db.ClaimReady(ctx, ClaimRequest{
		VersionID:       version.ID,
		TicketID:        1,
		Owner:           "agent-1",
		MaxParallelRuns: 1,
		LeaseTTL:        time.Minute,
		Now:             time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if claimed.TicketID != 1 || claimed.SessionID == "" || claimed.RunID == "" || claimed.LeaseToken == "" || claimed.LeaseGeneration != 1 {
		t.Fatalf("claim = %#v", claimed)
	}

	recovered, err := db.CurrentClaim(ctx, version.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.RunID != claimed.RunID || recovered.SessionID != claimed.SessionID || recovered.LeaseToken != claimed.LeaseToken {
		t.Fatalf("recovered = %#v, want %#v", recovered, claimed)
	}
	if _, err := db.ClaimReady(ctx, ClaimRequest{VersionID: version.ID, TicketID: 2, Owner: "agent-2", MaxParallelRuns: 2, LeaseTTL: time.Minute}); !errors.Is(err, ErrNotReady) {
		t.Fatalf("blocked ticket claim error = %v, want ErrNotReady", err)
	}
}

func TestConcurrentClaimsHaveOneWinnerAndOneFencingConflict(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	snapshot := testSnapshot()
	fingerprint, err := snapshot.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	version, err := db.BeginActivation(ctx, snapshot, fingerprint, "revision-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkActive(ctx, version.ID); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, owner := range []string{"agent-a", "agent-b"} {
		wg.Add(1)
		go func(owner string) {
			defer wg.Done()
			_, claimErr := db.ClaimReady(ctx, ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: owner, MaxParallelRuns: 2, LeaseTTL: time.Minute})
			results <- claimErr
		}(owner)
	}
	wg.Wait()
	close(results)

	var successes, conflicts int
	for claimErr := range results {
		switch {
		case claimErr == nil:
			successes++
		case errors.Is(claimErr, ErrFencingConflict):
			conflicts++
		default:
			t.Fatalf("claim error = %v, want nil or ErrFencingConflict", claimErr)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes = %d, conflicts = %d, want one each", successes, conflicts)
	}
}

func TestCapacityComparisonUsesFixedWidthLeaseTimestamps(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	snapshot := testSnapshot()
	snapshot.BlockedBy = map[int64][]plan.Issue{}
	fingerprint, err := snapshot.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	version, err := db.BeginActivation(ctx, snapshot, fingerprint, "revision-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkActive(ctx, version.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	if _, err := db.ClaimReady(ctx, ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent-1", MaxParallelRuns: 1, LeaseTTL: 100 * time.Millisecond, Now: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimReady(ctx, ClaimRequest{VersionID: version.ID, TicketID: 2, Owner: "agent-2", MaxParallelRuns: 1, LeaseTTL: time.Minute, Now: now}); !errors.Is(err, ErrCapacity) {
		t.Fatalf("second claim error = %v, want ErrCapacity", err)
	}
}

func TestNeedsAttentionTicketCannotBeClaimedAgain(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	snapshot := testSnapshot()
	snapshot.BlockedBy = map[int64][]plan.Issue{}
	fingerprint, err := snapshot.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	version, err := db.BeginActivation(ctx, snapshot, fingerprint, "revision-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkActive(ctx, version.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	claim, err := db.ClaimReady(ctx, ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent-1", MaxParallelRuns: 1, LeaseTTL: time.Minute, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `UPDATE ticket_runtime SET state = ? WHERE version_id = ? AND issue_id = ?`, plan.StateNeedsAttention, version.ID, claim.TicketID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `UPDATE worker_runs SET state = 'succeeded' WHERE run_id = ?`, claim.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `UPDATE run_leases SET state = 'revoked' WHERE run_id = ?`, claim.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `UPDATE ticket_sessions SET owner = '' WHERE session_id = ?`, claim.SessionID); err != nil {
		t.Fatal(err)
	}
	frontier, err := db.ReadyFrontier(ctx, version.ID, 1, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(frontier) != 1 || frontier[0].IssueID != 2 {
		t.Fatalf("frontier = %#v, want only ticket 2", frontier)
	}
	if _, err := db.ClaimReady(ctx, ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent-2", MaxParallelRuns: 1, LeaseTTL: time.Minute, Now: now.Add(time.Hour)}); !errors.Is(err, ErrFencingConflict) {
		t.Fatalf("needs-attention claim error = %v, want ErrFencingConflict", err)
	}
}

func TestMarkTicketDeliveredUnlocksDependentTicket(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	snapshot := testSnapshot()
	fingerprint, err := snapshot.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	version, err := db.BeginActivation(ctx, snapshot, fingerprint, "revision-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkActive(ctx, version.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimReady(ctx, ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent-1", MaxParallelRuns: 2, LeaseTTL: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkTicketDelivered(ctx, version.ID, 1); err != nil {
		t.Fatal(err)
	}
	frontier, err := db.ReadyFrontier(ctx, version.ID, 2, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(frontier) != 1 || frontier[0].IssueID != 2 {
		t.Fatalf("frontier after delivery = %#v, want ticket 2", frontier)
	}
	if _, err := db.CurrentClaim(ctx, version.ID, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delivered CurrentClaim error = %v, want ErrNotFound", err)
	}
}

func TestPlanProjectionUsesBlockerIssueNumbers(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	snapshot := testSnapshot()
	fingerprint, err := snapshot.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	version, err := db.BeginActivation(ctx, snapshot, fingerprint, "revision-1")
	if err != nil {
		t.Fatal(err)
	}
	projection, err := db.PlanProjection(ctx, version.ID)
	if err != nil {
		t.Fatal(err)
	}
	var blocked plan.ProjectionTicket
	for _, ticket := range projection.Tickets {
		if ticket.Number == 12 {
			blocked = ticket
		}
	}
	if len(blocked.Blockers) != 1 || blocked.Blockers[0] != 11 {
		t.Fatalf("projection blockers = %#v, want issue number 11", projection.Tickets)
	}
}

func TestPlanProjectionDoesNotTreatPRPublicationAsAGateResult(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	snapshot := testSnapshot()
	fingerprint, err := snapshot.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	version, err := db.BeginActivation(ctx, snapshot, fingerprint, "revision-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkActive(ctx, version.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	claim, err := db.ClaimReady(ctx, ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent-1", MaxParallelRuns: 1, LeaseTTL: time.Hour, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.BindAgent(ctx, AgentBinding{SessionID: claim.SessionID, AgentIdentity: "agent-1", WorkspacePath: "workspace", CodexStatePath: "codex", Branch: "ticket-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcceptCandidate(ctx, CandidateRevision{RunID: claim.RunID, LeaseToken: claim.LeaseToken, CodexSessionID: "codex-session", CommitSHA: "accepted", StructuredOutput: []byte(`{"summary":"candidate"}`), Now: now, Publication: CandidatePublication{Repository: "owner/repo", Branch: "ticket-1", ExpectRemoteAbsent: true, Title: "ticket"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, "UPDATE ticket_deliveries SET pull_request_number = 42 WHERE version_id = ? AND issue_id = ?", version.ID, int64(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, "UPDATE delivery_outbox SET state = ? WHERE operation = ?", OutboxSucceeded, DeliveryUpsertPR); err != nil {
		t.Fatal(err)
	}
	projection, err := db.PlanProjectionAt(ctx, version.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if projection.Tickets[0].GateResult != "not run" {
		t.Fatalf("gate result = %q, want not run", projection.Tickets[0].GateResult)
	}
}
