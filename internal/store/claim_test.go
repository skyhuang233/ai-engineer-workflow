package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

	now := time.Now().UTC()
	claimed, err := db.ClaimReady(ctx, ClaimRequest{
		VersionID:       version.ID,
		TicketID:        1,
		Owner:           "agent-1",
		MaxParallelRuns: 1,
		LeaseTTL:        time.Minute,
		Now:             now,
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

func TestClaimReadyCompensatesNewSessionProvisioningOnTransactionFailure(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db, err := Open(ctx, filepath.Join(root, "workflow.db"))
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
	if _, err := db.db.ExecContext(ctx, `CREATE TRIGGER reject_worker_run BEFORE INSERT ON worker_runs BEGIN SELECT RAISE(ABORT, 'injected worker run failure'); END`); err != nil {
		t.Fatal(err)
	}
	var authPath string
	rolledBack := false
	_, err = db.ClaimReady(ctx, ClaimRequest{
		VersionID: version.ID, TicketID: 1, Owner: "agent-1", MaxParallelRuns: 1, LeaseTTL: time.Minute,
		ProvisionSession: func(_ context.Context, provisioning SessionProvisioning) (SessionProvisioningResult, error) {
			authPath = filepath.Join(root, provisioning.SessionID, "auth.json")
			if err := os.MkdirAll(filepath.Dir(authPath), 0o755); err != nil {
				return SessionProvisioningResult{}, err
			}
			if err := os.WriteFile(authPath, []byte("credential"), 0o600); err != nil {
				return SessionProvisioningResult{}, err
			}
			return SessionProvisioningResult{Rollback: func() error {
				rolledBack = true
				return os.Remove(authPath)
			}}, nil
		},
	})
	if err == nil {
		t.Fatal("claim unexpectedly survived injected transaction failure")
	}
	if !rolledBack {
		t.Fatal("new Session authentication was not compensated")
	}
	if _, err := os.Stat(authPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("uncommitted authentication cache survived: %v", err)
	}
	if _, err := db.TicketSession(ctx, version.ID, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed claim retained Session: %v", err)
	}
}

func TestCurrentClaimRejectsExpiredLease(t *testing.T) {
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
	if _, err := db.ClaimReady(ctx, ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent-1", MaxParallelRuns: 1, LeaseTTL: time.Minute, Now: time.Now().UTC().Add(-2 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CurrentClaim(ctx, version.ID, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired CurrentClaim error = %v, want ErrNotFound", err)
	}
}

func TestRecordRunFailureDoesNotRequireDiagnosticEvidence(t *testing.T) {
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
	claim, err := db.ClaimReady(ctx, ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent", MaxParallelRuns: 1, LeaseTTL: time.Hour, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RecordRunFailure(ctx, RunFailure{RunID: claim.RunID, LeaseToken: claim.LeaseToken, Error: "worker failed without evidence", Now: now.Add(time.Second)}); err != nil {
		t.Fatalf("RecordRunFailure() = %v", err)
	}
	var runState string
	if err := db.db.QueryRowContext(ctx, `SELECT state FROM worker_runs WHERE run_id = ?`, claim.RunID).Scan(&runState); err != nil {
		t.Fatal(err)
	}
	if runState != "failed" {
		t.Fatalf("Run state = %q, want failed", runState)
	}
	if _, err := db.RunDiagnostic(ctx, claim.RunID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing evidence unexpectedly created a diagnostic: %v", err)
	}
}

func TestCandidateAcceptanceRejectsAdditionalStructuredOutputProperties(t *testing.T) {
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
	now := time.Now().UTC()
	claim, err := db.ClaimReady(ctx, ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent-1", MaxParallelRuns: 1, LeaseTTL: time.Hour, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.AcceptCandidateForDelivery(ctx, CandidateRevision{
		RunID: claim.RunID, LeaseToken: claim.LeaseToken, CodexSessionID: "codex-session", CommitSHA: "accepted",
		StructuredOutput: []byte(`{"summary":"candidate","unexpected":"value"}`), Now: now,
		Publication: CandidatePublication{Repository: snapshot.Repository, Branch: "ticket-1", ExpectRemoteAbsent: true, Title: "ticket"},
	}, time.Hour)
	if !errors.Is(err, ErrInvalidClaim) {
		t.Fatalf("candidate acceptance error = %v, want ErrInvalidClaim", err)
	}
	if _, err := db.CandidateRevision(ctx, claim.RunID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("invalid Candidate was persisted: %v", err)
	}
}

func TestExpiredDeliveryControllerNeedsAttentionBeforeAgentRecovery(t *testing.T) {
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
	delivery, err := db.AcceptCandidateForDelivery(ctx, CandidateRevision{RunID: claim.RunID, LeaseToken: claim.LeaseToken, CodexSessionID: "codex-session", CommitSHA: "accepted", StructuredOutput: []byte(`{"summary":"candidate","checks":[{"command":"go test","outcome":"passed"}]}`), Now: now, Publication: CandidatePublication{Repository: snapshot.Repository, Branch: "ticket-1", ExpectRemoteAbsent: true, Title: "ticket"}}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimReady(ctx, ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "replacement", MaxParallelRuns: 1, LeaseTTL: time.Hour, Now: now.Add(2 * time.Hour)}); !errors.Is(err, ErrNotReady) {
		t.Fatalf("expired delivery recovery = %v, want ErrNotReady", err)
	}
	var runtimeState, runState, leaseState string
	if err := db.db.QueryRowContext(ctx, `SELECT rt.state, r.state, l.state
FROM ticket_runtime rt
JOIN ticket_sessions s ON s.version_id = rt.version_id AND s.issue_id = rt.issue_id
JOIN worker_runs r ON r.run_id = s.current_run_id
JOIN run_leases l ON l.run_id = r.run_id AND l.generation = r.lease_generation
WHERE rt.version_id = ? AND rt.issue_id = ?`, version.ID, claim.TicketID).Scan(&runtimeState, &runState, &leaseState); err != nil {
		t.Fatal(err)
	}
	if runtimeState != plan.StateNeedsAttention || runState != "failed" || leaseState != "expired" {
		t.Fatalf("expired delivery state = runtime %q, run %q, lease %q", runtimeState, runState, leaseState)
	}
	if _, err := db.ClaimReady(ctx, ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "replacement", MaxParallelRuns: 1, LeaseTTL: time.Hour, Now: now.Add(3 * time.Hour)}); !errors.Is(err, ErrFencingConflict) {
		t.Fatalf("needs-attention recovery = %v, want ErrFencingConflict", err)
	}
	if _, err := db.CurrentClaim(ctx, version.ID, delivery.TicketID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired delivery current claim = %v, want ErrNotFound", err)
	}
}

func TestRecoveryExpiresDeliveryControllersBeforeReturningLiveRuns(t *testing.T) {
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
	if _, err := db.AcceptCandidateForDelivery(ctx, CandidateRevision{RunID: claim.RunID, LeaseToken: claim.LeaseToken, CodexSessionID: "codex-session", CommitSHA: "accepted", StructuredOutput: []byte(`{"summary":"candidate","checks":[{"command":"go test","outcome":"passed"}]}`), Now: now, Publication: CandidatePublication{Repository: snapshot.Repository, Branch: "ticket-1", ExpectRemoteAbsent: true, Title: "ticket"}}, time.Hour); err != nil {
		t.Fatal(err)
	}
	runs, err := db.ActiveRecoveryRuns(ctx, version.ID, now.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("recovery runs = %#v", runs)
	}
	projection, err := db.PlanProjectionAt(ctx, version.ID, now.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if projection.Tickets[0].State != "Needs Attention" || len(projection.Questions) != 1 {
		t.Fatalf("recovery projection = %#v", projection)
	}
	if _, err := db.ActiveRecoveryRuns(ctx, version.ID, now.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	questions, err := db.OpenWorkflowQuestions(ctx, snapshot.Repository, snapshot.Root.Number)
	if err != nil || len(questions) != 1 {
		t.Fatalf("recovery questions = %#v, %v", questions, err)
	}
}

func TestDeliveryClaimsCannotBeReadOrAcceptedAsAgentClaims(t *testing.T) {
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
	now := time.Now().UTC()
	claim, err := db.ClaimReady(ctx, ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent-1", MaxParallelRuns: 1, LeaseTTL: time.Hour, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := db.AcceptCandidateForDelivery(ctx, CandidateRevision{RunID: claim.RunID, LeaseToken: claim.LeaseToken, CodexSessionID: "codex-session", CommitSHA: "accepted", StructuredOutput: []byte(`{"summary":"candidate","checks":[{"command":"go test","outcome":"passed"}]}`), Now: now, Publication: CandidatePublication{Repository: snapshot.Repository, Branch: "ticket-1", ExpectRemoteAbsent: true, Title: "ticket"}}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.currentClaimAt(ctx, version.ID, claim.TicketID, now.Add(time.Second)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delivery CurrentClaim error = %v, want ErrNotFound", err)
	}
	if err := db.AcceptCandidate(ctx, CandidateRevision{RunID: delivery.RunID, LeaseToken: delivery.LeaseToken, CodexSessionID: "codex-session", CommitSHA: "replacement", StructuredOutput: []byte(`{"summary":"replacement"}`), Now: now.Add(time.Second), Publication: CandidatePublication{Repository: snapshot.Repository, Branch: "ticket-1", ExpectRemoteAbsent: true, Title: "ticket"}}); !errors.Is(err, ErrInvalidClaim) {
		t.Fatalf("delivery candidate acceptance error = %v, want ErrInvalidClaim", err)
	}
}

func TestExpiredDeliveryFeedbackNeedsAttentionWithoutAgentRevision(t *testing.T) {
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
	if _, err := db.AcceptCandidateForDelivery(ctx, CandidateRevision{RunID: claim.RunID, LeaseToken: claim.LeaseToken, CodexSessionID: "codex-session", CommitSHA: "accepted", StructuredOutput: []byte(`{"summary":"candidate","checks":[{"command":"go test","outcome":"passed"}]}`), Now: now, Publication: CandidatePublication{Repository: snapshot.Repository, Branch: "ticket-1", ExpectRemoteAbsent: true, Title: "ticket"}}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecordReviewFeedback(ctx, version.ID, claim.TicketID, []ReviewFeedback{{Source: "review", EventID: "1", Body: "Please revise."}}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.ClaimQueuedReviewRevision(ctx, version.ID, claim.TicketID, time.Hour, now.Add(2*time.Hour), 1, DefaultMaxWorkerAttempts); !errors.Is(err, ErrNeedsAttention) {
		t.Fatalf("expired delivery feedback recovery = %v, want ErrNeedsAttention", err)
	}
	var runtimeState string
	var agentRuns int
	if err := db.db.QueryRowContext(ctx, `SELECT state FROM ticket_runtime WHERE version_id = ? AND issue_id = ?`, version.ID, claim.TicketID).Scan(&runtimeState); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM worker_runs WHERE session_id = ? AND run_kind = ? AND state = ?`, claim.SessionID, RunAgent, RunRunning).Scan(&agentRuns); err != nil {
		t.Fatal(err)
	}
	if runtimeState != plan.StateNeedsAttention || agentRuns != 0 {
		t.Fatalf("expired delivery feedback state = %q, running agent revisions = %d", runtimeState, agentRuns)
	}
}

func TestDeliveryCompletionRequiresUnexpiredLease(t *testing.T) {
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
	delivery, err := db.AcceptCandidateForDelivery(ctx, CandidateRevision{RunID: claim.RunID, LeaseToken: claim.LeaseToken, CodexSessionID: "codex-session", CommitSHA: "accepted", StructuredOutput: []byte(`{"summary":"candidate","checks":[{"command":"go test","outcome":"passed"}]}`), Now: now, Publication: CandidatePublication{Repository: snapshot.Repository, Branch: "ticket-1", ExpectRemoteAbsent: true, Title: "ticket"}}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteDeliveryController(ctx, delivery, now.Add(2*time.Hour)); !errors.Is(err, ErrNeedsAttention) {
		t.Fatalf("late delivery completion error = %v, want ErrNeedsAttention", err)
	}
	var runState, leaseState string
	if err := db.db.QueryRowContext(ctx, `SELECT r.state, l.state FROM worker_runs r JOIN run_leases l ON l.run_id = r.run_id AND l.generation = r.lease_generation WHERE r.run_id = ?`, delivery.RunID).Scan(&runState, &leaseState); err != nil {
		t.Fatal(err)
	}
	if runState != "failed" || leaseState != "expired" {
		t.Fatalf("late completion state = run %q lease %q", runState, leaseState)
	}
	projection, err := db.PlanProjectionAt(ctx, version.ID, now.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if projection.Tickets[0].State != "Needs Attention" {
		t.Fatalf("late completion projection = %#v", projection)
	}
}

func TestExpiredCurrentAgentFailureTerminatesRun(t *testing.T) {
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
	claim, err := db.ClaimReady(ctx, ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent-1", MaxParallelRuns: 1, LeaseTTL: time.Minute, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RecordRunFailure(ctx, RunFailure{RunID: claim.RunID, LeaseToken: claim.LeaseToken, DiagnosticsPath: "diagnostics/late", Error: "late failure", Now: now.Add(2 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	diagnostic, err := db.RunDiagnostic(ctx, claim.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if diagnostic != "diagnostics/late" {
		t.Fatalf("diagnostics path = %q", diagnostic)
	}
	var runState, leaseState string
	var failures int
	if err := db.db.QueryRowContext(ctx, `SELECT r.state, l.state, s.consecutive_failures
FROM worker_runs r JOIN run_leases l ON l.run_id = r.run_id AND l.generation = r.lease_generation
JOIN ticket_sessions s ON s.session_id = r.session_id
WHERE r.run_id = ?`, claim.RunID).Scan(&runState, &leaseState, &failures); err != nil {
		t.Fatal(err)
	}
	if runState != "failed" || leaseState != "revoked" || failures != 1 {
		t.Fatalf("late current agent failure left run=%q lease=%q failures=%d", runState, leaseState, failures)
	}
}

func TestExpiredAgentOutputCannotBindCodexSession(t *testing.T) {
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
	now := time.Now().UTC()
	claim, err := db.ClaimReady(ctx, ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent-1", MaxParallelRuns: 1, LeaseTTL: time.Hour, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `UPDATE run_leases SET expires_at = ? WHERE run_id = ?`, formatTimestamp(now.Add(-time.Minute)), claim.RunID); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordCodexSession(ctx, claim.RunID, claim.LeaseToken, "late-session"); !errors.Is(err, ErrInvalidClaim) {
		t.Fatalf("late Codex session error = %v, want ErrInvalidClaim", err)
	}
	session, err := db.TicketSession(ctx, version.ID, claim.TicketID)
	if err != nil {
		t.Fatal(err)
	}
	if session.CodexSessionID != "" {
		t.Fatalf("late Codex session mutated ticket session = %q", session.CodexSessionID)
	}
}

func TestReserveWorkerLaunchAllowsOnlyOneStarter(t *testing.T) {
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
	now := time.Now().UTC()
	claim, err := db.ClaimReady(ctx, ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent-1", MaxParallelRuns: 1, LeaseTTL: time.Minute, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ReserveWorkerLaunch(ctx, claim, now); err != nil {
		t.Fatal(err)
	}
	if err := db.ReserveWorkerLaunch(ctx, claim, now); !errors.Is(err, ErrWorkerLaunched) {
		t.Fatalf("second launch reservation = %v, want ErrWorkerLaunched", err)
	}
}

func TestClaimReadyStopsAfterConfiguredAttemptLimit(t *testing.T) {
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
	now := time.Now().UTC()
	var claim TicketClaim
	for attempt := 0; attempt < 2; attempt++ {
		claim, err = db.ClaimReady(ctx, ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent-1", MaxParallelRuns: 1, MaxAttempts: 2, LeaseTTL: time.Hour, Now: now.Add(time.Duration(attempt) * time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.RecordRunFailure(ctx, RunFailure{RunID: claim.RunID, LeaseToken: claim.LeaseToken, DiagnosticsPath: "diagnostics", Error: "failed", Now: now.Add(time.Duration(attempt)*time.Minute + time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ClaimReady(ctx, ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent-1", MaxParallelRuns: 1, MaxAttempts: 2, LeaseTTL: time.Hour, Now: now.Add(3 * time.Minute)}); !errors.Is(err, ErrNotReady) {
		t.Fatalf("attempt beyond limit = %v, want ErrNotReady", err)
	}
	projection, err := db.PlanProjection(ctx, version.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.Tickets) == 0 || projection.Tickets[0].State != "Needs Attention" {
		t.Fatalf("projection tickets = %#v, want Needs Attention", projection.Tickets)
	}
	if len(projection.Questions) != 1 || !strings.Contains(projection.Questions[0].Prompt, "retry budget exhausted") {
		t.Fatalf("workflow inbox = %#v", projection.Questions)
	}
}

func TestReviewFeedbackDeduplicatesAndBatchesOneRevision(t *testing.T) {
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
	now := time.Now().UTC()
	claim, err := db.ClaimReady(ctx, ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent-1", MaxParallelRuns: 1, LeaseTTL: time.Hour, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `UPDATE worker_runs SET state = 'succeeded' WHERE run_id = ?`, claim.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `UPDATE run_leases SET state = 'revoked' WHERE run_id = ?`, claim.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `UPDATE ticket_runtime SET state = ? WHERE version_id = ? AND issue_id = ?`, plan.StateWaitingReview, version.ID, claim.TicketID); err != nil {
		t.Fatal(err)
	}
	inserted, err := db.RecordReviewFeedback(ctx, version.ID, claim.TicketID, []ReviewFeedback{{Source: "review", EventID: "100", Author: "human", Body: "Please rename this."}, {Source: "review", EventID: "100", Author: "human", Body: "Please rename this."}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if inserted != 1 {
		t.Fatalf("inserted = %d, want 1", inserted)
	}
	revision, prompt, err := db.ClaimQueuedReviewRevision(ctx, version.ID, claim.TicketID, time.Hour, now.Add(time.Second), 1, DefaultMaxWorkerAttempts)
	if err != nil {
		t.Fatal(err)
	}
	if revision.SessionID != claim.SessionID || revision.Attempt != claim.Attempt+1 || !strings.Contains(prompt, "Please rename this.") {
		t.Fatalf("revision = %#v, prompt = %q", revision, prompt)
	}
	inserted, err = db.RecordReviewFeedback(ctx, version.ID, claim.TicketID, []ReviewFeedback{{Source: "review", EventID: "100", Author: "human", Body: "Please rename this."}}, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if inserted != 0 {
		t.Fatalf("repeated event inserted = %d, want 0", inserted)
	}
}

func TestReviewRevisionsDoNotConsumeFailureRecoveryBudget(t *testing.T) {
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
	now := time.Now().UTC()
	claim, err := db.ClaimReady(ctx, ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent-1", MaxParallelRuns: 1, LeaseTTL: time.Hour, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	for round := 1; round <= 3; round++ {
		if _, err := db.db.ExecContext(ctx, `UPDATE worker_runs SET state = 'succeeded' WHERE run_id = ?`, claim.RunID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.db.ExecContext(ctx, `UPDATE run_leases SET state = 'revoked' WHERE run_id = ?`, claim.RunID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.db.ExecContext(ctx, `UPDATE ticket_runtime SET state = ? WHERE version_id = ? AND issue_id = ?`, plan.StateWaitingReview, version.ID, claim.TicketID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.RecordReviewFeedback(ctx, version.ID, claim.TicketID, []ReviewFeedback{{Source: "review", EventID: fmt.Sprint(round), Author: "human", Body: "Please revise."}}, now.Add(time.Duration(round)*time.Second)); err != nil {
			t.Fatal(err)
		}
		claim, _, err = db.ClaimQueuedReviewRevision(ctx, version.ID, claim.TicketID, time.Hour, now.Add(time.Duration(round)*time.Second), 1, 1)
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
	}
	if claim.Attempt != 4 {
		t.Fatalf("review claim = %#v, want fourth run", claim)
	}
}

func TestFailedReviewRevisionRequeuesItsFeedback(t *testing.T) {
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
	now := time.Now().UTC()
	claim, err := db.ClaimReady(ctx, ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent-1", MaxParallelRuns: 1, LeaseTTL: time.Hour, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `UPDATE worker_runs SET state = 'succeeded' WHERE run_id = ?`, claim.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `UPDATE run_leases SET state = 'revoked' WHERE run_id = ?`, claim.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `UPDATE ticket_runtime SET state = ? WHERE version_id = ? AND issue_id = ?`, plan.StateWaitingReview, version.ID, claim.TicketID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecordReviewFeedback(ctx, version.ID, claim.TicketID, []ReviewFeedback{{Source: "review", EventID: "100", Author: "human", Body: "Please rename this."}}, now); err != nil {
		t.Fatal(err)
	}
	revision, _, err := db.ClaimQueuedReviewRevision(ctx, version.ID, claim.TicketID, time.Hour, now.Add(time.Second), 1, DefaultMaxWorkerAttempts)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RecordRunFailure(ctx, RunFailure{RunID: revision.RunID, LeaseToken: revision.LeaseToken, DiagnosticsPath: "diagnostics", Error: "worker failed", Now: now.Add(2 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	retry, prompt, err := db.ClaimQueuedReviewRevision(ctx, version.ID, claim.TicketID, time.Hour, now.Add(3*time.Second), 1, DefaultMaxWorkerAttempts)
	if err != nil {
		t.Fatal(err)
	}
	if retry.Attempt != revision.Attempt+1 || !strings.Contains(prompt, "Please rename this.") {
		t.Fatalf("retry = %#v, prompt = %q", retry, prompt)
	}
}

func TestReviewAuthenticationFailureReleasesClaimedFeedback(t *testing.T) {
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
	now := time.Now().UTC()
	claim, err := db.ClaimReady(ctx, ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent-1", MaxParallelRuns: 1, LeaseTTL: time.Hour, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `UPDATE worker_runs SET state = 'succeeded' WHERE run_id = ?`, claim.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `UPDATE run_leases SET state = 'revoked' WHERE run_id = ?`, claim.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `UPDATE ticket_runtime SET state = ? WHERE version_id = ? AND issue_id = ?`, plan.StateWaitingReview, version.ID, claim.TicketID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecordReviewFeedback(ctx, version.ID, claim.TicketID, []ReviewFeedback{{Source: "review", EventID: "auth-failure", Author: "human", Body: "Please revise."}}, now); err != nil {
		t.Fatal(err)
	}
	revision, _, err := db.ClaimQueuedReviewRevision(ctx, version.ID, claim.TicketID, time.Hour, now.Add(time.Second), 1, DefaultMaxWorkerAttempts)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RecordRunFailure(ctx, RunFailure{RunID: revision.RunID, LeaseToken: revision.LeaseToken, Error: ErrSessionAuthenticationUnavailable.Error(), Now: now.Add(2 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	var claimedRunID string
	if err := db.db.QueryRowContext(ctx, `SELECT claimed_run_id FROM review_feedback_events WHERE version_id = ? AND issue_id = ? AND source = ? AND event_id = ?`, version.ID, claim.TicketID, "review", "auth-failure").Scan(&claimedRunID); err != nil {
		t.Fatal(err)
	}
	if claimedRunID != "" {
		t.Fatalf("authentication-failed review feedback remained claimed by %q", claimedRunID)
	}
	projection, err := db.PlanProjection(ctx, version.ID)
	if err != nil {
		t.Fatal(err)
	}
	if projection.Tickets[0].State != "Needs Attention" {
		t.Fatalf("authentication-failed review projection = %#v", projection.Tickets)
	}
}

func TestExpiredReviewRevisionRequeuesItsFeedback(t *testing.T) {
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
	if _, err := db.db.ExecContext(ctx, `UPDATE worker_runs SET state = 'succeeded' WHERE run_id = ?`, claim.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `UPDATE run_leases SET state = 'revoked' WHERE run_id = ?`, claim.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `UPDATE ticket_runtime SET state = ? WHERE version_id = ? AND issue_id = ?`, plan.StateWaitingReview, version.ID, claim.TicketID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecordReviewFeedback(ctx, version.ID, claim.TicketID, []ReviewFeedback{{Source: "review", EventID: "100", Author: "human", Body: "Please rename this."}}, now); err != nil {
		t.Fatal(err)
	}
	revision, _, err := db.ClaimQueuedReviewRevision(ctx, version.ID, claim.TicketID, time.Minute, now.Add(time.Second), 1, DefaultMaxWorkerAttempts)
	if err != nil {
		t.Fatal(err)
	}
	retry, prompt, err := db.ClaimQueuedReviewRevision(ctx, version.ID, claim.TicketID, time.Hour, now.Add(2*time.Hour), 1, DefaultMaxWorkerAttempts)
	if err != nil {
		t.Fatal(err)
	}
	if retry.Attempt != revision.Attempt+1 || !strings.Contains(prompt, "Please rename this.") {
		t.Fatalf("retry = %#v, prompt = %q", retry, prompt)
	}
	var consecutiveFailures int
	if err := db.db.QueryRowContext(ctx, `SELECT consecutive_failures FROM ticket_sessions WHERE session_id = ?`, retry.SessionID).Scan(&consecutiveFailures); err != nil {
		t.Fatal(err)
	}
	if consecutiveFailures != 1 {
		t.Fatalf("expired revision failures = %d, want 1", consecutiveFailures)
	}
}

func TestExpiredReviewRevisionConsumesRetryBudget(t *testing.T) {
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
	if _, err := db.db.ExecContext(ctx, `UPDATE worker_runs SET state = 'succeeded' WHERE run_id = ?`, claim.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `UPDATE run_leases SET state = 'revoked' WHERE run_id = ?`, claim.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `UPDATE ticket_runtime SET state = ? WHERE version_id = ? AND issue_id = ?`, plan.StateWaitingReview, version.ID, claim.TicketID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecordReviewFeedback(ctx, version.ID, claim.TicketID, []ReviewFeedback{{Source: "review", EventID: "100", Author: "human", Body: "Please rename this."}}, now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.ClaimQueuedReviewRevision(ctx, version.ID, claim.TicketID, time.Minute, now.Add(time.Second), 1, 1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.ClaimQueuedReviewRevision(ctx, version.ID, claim.TicketID, time.Hour, now.Add(2*time.Hour), 1, 1); !errors.Is(err, ErrNotReady) {
		t.Fatalf("expired revision retry budget error = %v, want ErrNotReady", err)
	}
	var runtimeState string
	var consecutiveFailures int
	if err := db.db.QueryRowContext(ctx, `SELECT rt.state, s.consecutive_failures FROM ticket_runtime rt JOIN ticket_sessions s ON s.version_id = rt.version_id AND s.issue_id = rt.issue_id WHERE rt.version_id = ? AND rt.issue_id = ?`, version.ID, claim.TicketID).Scan(&runtimeState, &consecutiveFailures); err != nil {
		t.Fatal(err)
	}
	if runtimeState != plan.StateNeedsAttention || consecutiveFailures != 1 {
		t.Fatalf("expired revision budget state = %q, failures = %d", runtimeState, consecutiveFailures)
	}
}

func TestAgentRevisionCannotSubmitEvidenceBeforeDeliveryHandoff(t *testing.T) {
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
	claim, err := db.ClaimReady(ctx, ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent", MaxParallelRuns: 1, LeaseTTL: time.Hour, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.BindAgent(ctx, AgentBinding{SessionID: claim.SessionID, AgentIdentity: "agent", WorkspacePath: "workspace", CodexStatePath: "codex", Branch: "ticket-1"}); err != nil {
		t.Fatal(err)
	}
	delivery, err := db.AcceptCandidateForDelivery(ctx, CandidateRevision{RunID: claim.RunID, LeaseToken: claim.LeaseToken, CodexSessionID: "codex", CommitSHA: "accepted", StructuredOutput: []byte(`{"summary":"candidate","checks":[{"command":"go test","outcome":"passed"}]}`), Now: now, Publication: CandidatePublication{Repository: snapshot.Repository, Branch: "ticket-1", ExpectRemoteAbsent: true, Title: "ticket"}}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.EnqueueDelivery(ctx, DeliveryRequest{Operation: DeliveryPushCandidate, RunID: delivery.RunID, LeaseToken: delivery.LeaseToken, LeaseGeneration: delivery.LeaseGeneration, Repository: snapshot.Repository, Branch: "ticket-1", CommitSHA: "accepted", ExpectRemoteAbsent: true}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `UPDATE ticket_deliveries SET pull_request_number = 42 WHERE version_id = ? AND issue_id = ?`, version.ID, int64(1)); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteDeliveryController(ctx, delivery, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecordReviewFeedback(ctx, version.ID, 1, []ReviewFeedback{{Source: "review", EventID: "1", Author: "human", Body: "Please revise."}}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	revision, _, err := db.ClaimQueuedReviewRevision(ctx, version.ID, 1, time.Hour, now.Add(2*time.Second), 1, DefaultMaxWorkerAttempts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.EnqueueDelivery(ctx, DeliveryRequest{Operation: DeliveryReplyEvidence, RunID: revision.RunID, LeaseToken: revision.LeaseToken, LeaseGeneration: revision.LeaseGeneration, Repository: snapshot.Repository, Branch: "ticket-1", PullRequestNumber: 42, Evidence: "mutable evidence"}, now.Add(3*time.Second)); !errors.Is(err, ErrDeliveryRejected) {
		t.Fatalf("Agent revision evidence error = %v, want ErrDeliveryRejected", err)
	}
}

func TestAcceptedHandoffRejectsMutableDeliveryAfterNeedsAttention(t *testing.T) {
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
	claim, err := db.ClaimReady(ctx, ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent", MaxParallelRuns: 1, LeaseTTL: time.Hour, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.BindAgent(ctx, AgentBinding{SessionID: claim.SessionID, AgentIdentity: "agent", WorkspacePath: "workspace", CodexStatePath: "codex", Branch: "ticket-1"}); err != nil {
		t.Fatal(err)
	}
	if err := db.AcceptCandidate(ctx, CandidateRevision{RunID: claim.RunID, LeaseToken: claim.LeaseToken, CodexSessionID: "codex", CommitSHA: "accepted", StructuredOutput: []byte(`{"summary":"candidate","checks":[{"command":"go test","outcome":"passed"}]}`), Now: now, Publication: CandidatePublication{Repository: snapshot.Repository, Branch: "ticket-1", ExpectRemoteAbsent: true, Title: "ticket"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `UPDATE ticket_deliveries SET pull_request_number = 42 WHERE version_id = ? AND issue_id = ?`, version.ID, int64(1)); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkRepositoryNeedsAttention(ctx, snapshot.Repository, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.EnqueueDelivery(ctx, DeliveryRequest{Operation: DeliveryReplyEvidence, RunID: claim.RunID, LeaseToken: claim.LeaseToken, LeaseGeneration: claim.LeaseGeneration, Repository: snapshot.Repository, Branch: "ticket-1", PullRequestNumber: 42, Evidence: "mutable evidence"}, now.Add(2*time.Second)); !errors.Is(err, ErrDeliveryRejected) {
		t.Fatalf("mutable delivery error = %v, want rejected", err)
	}
}

func TestClosedPullRequestFreezesPlan(t *testing.T) {
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
	if _, err := db.ClaimReady(ctx, ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent-1", MaxParallelRuns: 1, LeaseTTL: time.Hour}); err != nil {
		t.Fatal(err)
	}
	frozen, err := db.FreezePlanForClosedPullRequest(ctx, version.ID, 1, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if !frozen {
		t.Fatal("plan was not frozen")
	}
	projection, err := db.PlanProjection(ctx, version.ID)
	if err != nil {
		t.Fatal(err)
	}
	if projection.State != "Needs Attention" {
		t.Fatalf("projection state = %q", projection.State)
	}
	frontier, err := db.ReadyFrontier(ctx, version.ID, 1, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(frontier) != 0 {
		t.Fatalf("frozen frontier = %#v", frontier)
	}
	questions, err := db.OpenWorkflowQuestions(ctx, snapshot.Repository, snapshot.Root.Number)
	if err != nil {
		t.Fatal(err)
	}
	var impact WorkflowQuestion
	for _, question := range questions {
		if question.Kind == "closed_unmerged_impact" {
			impact = question
			break
		}
	}
	if impact.ID == "" || !strings.Contains(impact.Prompt, "Tickets:") || !strings.Contains(impact.Prompt, "Merged work:") || !strings.Contains(impact.Prompt, "Cross-plan dependencies:") {
		t.Fatalf("closed pull request impact report = %#v", impact)
	}
}

func TestClosedPullRequestImpactReportIncludesUnstartedTickets(t *testing.T) {
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
	if _, err := db.db.ExecContext(ctx, `DELETE FROM ticket_runtime WHERE version_id = ? AND issue_id = ?`, version.ID, int64(2)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimReady(ctx, ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent-1", MaxParallelRuns: 1, LeaseTTL: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.FreezePlanForClosedPullRequest(ctx, version.ID, 1, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	questions, err := db.OpenWorkflowQuestions(ctx, snapshot.Repository, snapshot.Root.Number)
	if err != nil {
		t.Fatal(err)
	}
	for _, question := range questions {
		if question.Kind == "closed_unmerged_impact" {
			if !strings.Contains(question.Prompt, "ticket #12 (second): queued; agent unassigned; pull request none") {
				t.Fatalf("impact report omitted queued ticket: %q", question.Prompt)
			}
			return
		}
	}
	t.Fatal("closed-unmerged impact report was not recorded")
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

func TestDeliveryControllerCountsTowardClaimCapacity(t *testing.T) {
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
	now := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	claim, err := db.ClaimReady(ctx, ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent-1", MaxParallelRuns: 1, LeaseTTL: time.Hour, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcceptCandidateForDelivery(ctx, CandidateRevision{RunID: claim.RunID, LeaseToken: claim.LeaseToken, CodexSessionID: "codex", CommitSHA: "accepted", StructuredOutput: []byte(`{"summary":"candidate","checks":[{"command":"go test","outcome":"passed"}]}`), Now: now, Publication: CandidatePublication{Repository: snapshot.Repository, Branch: "ticket-1", ExpectRemoteAbsent: true, Title: "ticket"}}, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimReady(ctx, ClaimRequest{VersionID: version.ID, TicketID: 2, Owner: "agent-2", MaxParallelRuns: 1, LeaseTTL: time.Hour, Now: now.Add(time.Second)}); !errors.Is(err, ErrCapacity) {
		t.Fatalf("claim while delivery is running = %v, want ErrCapacity", err)
	}
}

func TestDeliveryControllerCountsTowardReviewRevisionCapacity(t *testing.T) {
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
	now := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	deliveryAgent, err := db.ClaimReady(ctx, ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent-1", MaxParallelRuns: 1, LeaseTTL: time.Hour, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcceptCandidateForDelivery(ctx, CandidateRevision{RunID: deliveryAgent.RunID, LeaseToken: deliveryAgent.LeaseToken, CodexSessionID: "codex-1", CommitSHA: "accepted", StructuredOutput: []byte(`{"summary":"candidate","checks":[{"command":"go test","outcome":"passed"}]}`), Now: now, Publication: CandidatePublication{Repository: snapshot.Repository, Branch: "ticket-1", ExpectRemoteAbsent: true, Title: "ticket-1"}}, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `INSERT INTO ticket_sessions(session_id, version_id, issue_id, owner, state, current_run_id, current_lease_generation, created_at, updated_at) VALUES (?, ?, ?, ?, ?, '', 0, ?, ?)`, "review-session", version.ID, int64(2), "agent-2", SessionRunning, formatTimestamp(now), formatTimestamp(now)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `UPDATE ticket_runtime SET state = ? WHERE version_id = ? AND issue_id = ?`, plan.StateWaitingReview, version.ID, int64(2)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecordReviewFeedback(ctx, version.ID, 2, []ReviewFeedback{{Source: "review", EventID: "1", Body: "Please revise."}}, now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.ClaimQueuedReviewRevision(ctx, version.ID, 2, time.Hour, now.Add(time.Second), 1, DefaultMaxWorkerAttempts); !errors.Is(err, ErrCapacity) {
		t.Fatalf("review revision while delivery is running = %v, want ErrCapacity", err)
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
	delivery, err := db.AcceptCandidateForDelivery(ctx, CandidateRevision{RunID: claim.RunID, LeaseToken: claim.LeaseToken, CodexSessionID: "codex-session", CommitSHA: "accepted", StructuredOutput: []byte(`{"summary":"candidate","checks":[{"command":"go test","outcome":"passed"}]}`), Now: now, Publication: CandidatePublication{Repository: "owner/repo", Branch: "ticket-1", ExpectRemoteAbsent: true, Title: "ticket"}}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.EnqueueDelivery(ctx, DeliveryRequest{Operation: DeliveryUpsertPR, RunID: delivery.RunID, LeaseToken: delivery.LeaseToken, LeaseGeneration: delivery.LeaseGeneration, Repository: "owner/repo", Branch: "ticket-1", CommitSHA: "accepted", ExpectedRemoteHead: "accepted", Title: "ticket"}, now.Add(time.Second)); err != nil {
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
