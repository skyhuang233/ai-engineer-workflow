package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/skyhuang233/workflow/internal/credential"
	"github.com/skyhuang233/workflow/internal/delivery"
	"github.com/skyhuang233/workflow/internal/github"
	"github.com/skyhuang233/workflow/internal/plan"
	"github.com/skyhuang233/workflow/internal/store"
)

func TestShouldPauseGatewayForCredential(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "missing", err: credential.ErrNotFound, want: true},
		{name: "rejected", err: fmt.Errorf("%w: fingerprint mismatch", delivery.ErrGatewayCredentialRejected), want: true},
		{name: "transient store error", err: errors.New("database temporarily unavailable")},
		{name: "cancelled", err: context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := shouldPauseGatewayForCredential(test.err); got != test.want {
				t.Fatalf("should pause = %t, want %t", got, test.want)
			}
		})
	}
}

func TestGatewayControlURLUsesHostOverrideAndPreservesLegacyFallback(t *testing.T) {
	workerURL := "http://host.docker.internal:8787"
	if got := gatewayControlURL(workerURL, ""); got != workerURL {
		t.Fatalf("legacy Gateway control URL = %q, want %q", got, workerURL)
	}
	controlURL := "http://127.0.0.1:8787"
	if got := gatewayControlURL(workerURL, controlURL); got != controlURL {
		t.Fatalf("host Gateway control URL = %q, want %q", got, controlURL)
	}
}

func TestMissingGatewayCredentialVerificationIsRejected(t *testing.T) {
	err := gatewayCredentialVerificationError(store.ErrNotFound)
	if !errors.Is(err, delivery.ErrGatewayCredentialRejected) {
		t.Fatalf("missing verification error = %v, want rejected credential", err)
	}
	if !shouldPauseGatewayForCredential(err) {
		t.Fatal("missing verification credential error did not pause Gateway writes")
	}
}

func TestGatewayCredentialVerificationReadFailureIsRetryable(t *testing.T) {
	err := gatewayCredentialVerificationError(errors.New("database temporarily unavailable"))
	if errors.Is(err, delivery.ErrGatewayCredentialRejected) {
		t.Fatalf("verification read failure = %v, want retryable error", err)
	}
	if shouldPauseGatewayForCredential(err) {
		t.Fatal("verification read failure paused Gateway writes")
	}
}

func TestPersistGatewayCredentialPauseCreatesLocalRecoveryInbox(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	snapshot := plan.Snapshot{
		Repository: "owner/repo",
		Root:       plan.Issue{ID: 100, Number: 10, Labels: []string{plan.PlanLabel}},
		Children:   []plan.Issue{{ID: 1, Number: 11, Title: "ticket", Labels: []string{plan.TicketLabel}, State: "open"}},
	}
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
	now := time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC)
	credentialErr := fmt.Errorf("load credential: %w", credential.ErrNotFound)
	if err := persistGatewayCredentialPause(ctx, db, credentialErr, now); !errors.Is(err, credential.ErrNotFound) {
		t.Fatalf("credential pause error = %v", err)
	}
	paused, _, err := db.GatewayWritesPaused(ctx)
	if err != nil || !paused {
		t.Fatalf("Gateway writes paused = %t, %v", paused, err)
	}
	inbox, err := db.WorkflowInboxItem(ctx, store.GatewayCredentialInboxKey)
	if err != nil || inbox.State != "open" {
		t.Fatalf("credential recovery inbox = %#v, %v", inbox, err)
	}
	questions, err := db.OpenWorkflowQuestions(ctx, "owner/repo", 10)
	if err != nil || len(questions) != 1 {
		t.Fatalf("credential recovery questions = %#v, %v", questions, err)
	}
}

func TestPersistGatewayCredentialPauseLeavesTransientFailuresRetryable(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	transient := errors.New("Credential Manager temporarily unavailable")
	if err := persistGatewayCredentialPause(ctx, db, transient, time.Now().UTC()); !errors.Is(err, transient) {
		t.Fatalf("transient credential error = %v", err)
	}
	paused, _, err := db.GatewayWritesPaused(ctx)
	if err != nil || paused {
		t.Fatalf("Gateway writes paused = %t, %v", paused, err)
	}
}

func TestPersistGatewayCredentialAdmissionErrorPausesForRejectedGitHubCredential(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	snapshot := plan.Snapshot{
		Repository: "owner/repo",
		Root:       plan.Issue{ID: 100, Number: 10, Labels: []string{plan.PlanLabel}},
		Children:   []plan.Issue{{ID: 1, Number: 11, Title: "ticket", Labels: []string{plan.TicketLabel}, State: "open"}},
	}
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
	now := time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC)
	pollErr := fmt.Errorf("repository admission: %w", &github.APIError{StatusCode: http.StatusUnauthorized})
	if err := persistGatewayCredentialAdmissionError(ctx, db, pollErr, now); !errors.Is(err, delivery.ErrGatewayCredentialRejected) {
		t.Fatalf("poll credential error = %v", err)
	}
	paused, _, err := db.GatewayWritesPaused(ctx)
	if err != nil || !paused {
		t.Fatalf("Gateway writes paused = %t, %v", paused, err)
	}
	inbox, err := db.WorkflowInboxItem(ctx, store.GatewayCredentialInboxKey)
	if err != nil || inbox.State != "open" {
		t.Fatalf("credential recovery inbox = %#v, %v", inbox, err)
	}
}

func TestPersistGatewayCredentialAdmissionErrorLeavesRateLimitsRetryable(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	retryAt := time.Date(2026, 8, 4, 0, 1, 0, 0, time.UTC)
	pollErr := &github.APIError{StatusCode: http.StatusForbidden, RetryAt: retryAt}
	if err := persistGatewayCredentialAdmissionError(ctx, db, pollErr, time.Now().UTC()); !errors.Is(err, pollErr) {
		t.Fatalf("rate limited poll error = %v", err)
	}
	paused, _, err := db.GatewayWritesPaused(ctx)
	if err != nil || paused {
		t.Fatalf("Gateway writes paused = %t, %v", paused, err)
	}
}

func TestRequireOwnerGuardedControlPlaneRepositoryAcceptsPrivateRepository(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/owner/repo" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"full_name":"owner/repo","owner":{"login":"owner"},"private":true}`))
	}))
	defer server.Close()
	err := requireOwnerGuardedControlPlaneRepository(context.Background(), github.NewClient(server.URL, "", server.Client()).WithRepositoryOwner("owner"), "owner/repo")
	if err != nil {
		t.Fatalf("private repository admission error = %v", err)
	}
}

func TestAcquireTicketClaimReplacesExpiredWorker(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	snapshot := plan.Snapshot{
		Repository: "owner/repo",
		Root:       plan.Issue{ID: 100, Number: 10, Labels: []string{plan.PlanLabel}},
		Children:   []plan.Issue{{ID: 1, Number: 11, Title: "first", Labels: []string{plan.TicketLabel}, State: "open"}},
	}
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
	expired, err := db.ClaimReady(ctx, store.ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent-1", MaxParallelRuns: 1, LeaseTTL: time.Minute, Now: now.Add(-2 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.BindAgent(ctx, store.AgentBinding{SessionID: expired.SessionID, AgentIdentity: "agent-1", WorkspacePath: "workspace", CodexStatePath: "codex", Branch: "ticket-1"}); err != nil {
		t.Fatal(err)
	}
	replacement, prompt, err := acquireTicketClaim(ctx, db, version.ID, expired.TicketID, store.DefaultMaxWorkerAttempts, now)
	if err != nil {
		t.Fatal(err)
	}
	if prompt != "" || replacement.SessionID != expired.SessionID || replacement.Attempt != expired.Attempt+1 || replacement.LeaseGeneration != expired.LeaseGeneration+1 {
		t.Fatalf("replacement = %#v, prompt = %q", replacement, prompt)
	}
}

func TestDispatchPendingDeliveryClaimsOnlyLaunchesRecoveredDelivery(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	snapshot := plan.Snapshot{
		Repository: "owner/repo",
		Root:       plan.Issue{ID: 100, Number: 10, Labels: []string{plan.PlanLabel}},
		Children:   []plan.Issue{{ID: 1, Number: 11, Title: "first", Labels: []string{plan.TicketLabel}, State: "open"}},
	}
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
	claim, err := db.ClaimReady(ctx, store.ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent", MaxParallelRuns: 1, LeaseTTL: time.Hour, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := db.AcceptCandidateForDelivery(ctx, store.CandidateRevision{RunID: claim.RunID, LeaseToken: claim.LeaseToken, CodexSessionID: "codex", CommitSHA: "accepted", StructuredOutput: []byte(`{"summary":"candidate","checks":[{"command":"go test","outcome":"passed"}]}`), Now: now, Publication: store.CandidatePublication{Repository: snapshot.Repository, Branch: "ticket-1", ExpectRemoteAbsent: true, Title: "ticket"}}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FailDeliveryController(ctx, delivery, "delivery failed", now.Add(time.Second)); err != nil {
		t.Fatalf("failed delivery = %v", err)
	}
	questions, err := db.OpenWorkflowQuestions(ctx, snapshot.Repository, snapshot.Root.Number)
	if err != nil || len(questions) != 1 {
		t.Fatalf("questions = %#v, %v", questions, err)
	}
	if err := db.AnswerWorkflowQuestion(ctx, snapshot.Repository, questions[0].ID, "retry", now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	launched := make(chan store.TicketClaim, 1)
	if err := dispatchPendingDeliveryClaims(ctx, db, snapshot.Repository, 1, time.Hour, now.Add(2*time.Second), func(_ context.Context, retry store.TicketClaim) error {
		launched <- retry
		return nil
	}, nil, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case retry := <-launched:
		if retry.RunID == claim.RunID || retry.RunID == "" {
			t.Fatalf("launched delivery claim = %#v", retry)
		}
	case <-time.After(time.Second):
		t.Fatal("recovered delivery was not launched")
	}
}

func TestLaunchDeliveryClaimsDoesNotBlockControlLoop(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{})
	claims := []store.TicketClaim{{RunID: "delivery-1"}, {RunID: "delivery-2"}}
	if err := launchDeliveryClaims(context.Background(), claims, func(_ context.Context, claim store.TicketClaim) error {
		started <- claim.RunID
		<-release
		return nil
	}, nil, nil); err != nil {
		t.Fatalf("launch delivery claims: %v", err)
	}
	for range claims {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("delivery claims did not launch concurrently")
		}
	}
	close(release)
}

func TestLaunchDeliveryClaimsJoinsOnceModeFailures(t *testing.T) {
	var workers sync.WaitGroup
	observed := make(chan error, 1)
	if err := launchDeliveryClaims(context.Background(), []store.TicketClaim{{RunID: "delivery-1"}}, func(context.Context, store.TicketClaim) error {
		return errors.New("delivery failed")
	}, &workers, func(err error) { observed <- err }); err != nil {
		t.Fatal(err)
	}
	workers.Wait()
	if err := <-observed; err == nil || err.Error() != "delivery failed" {
		t.Fatalf("observed failure = %v", err)
	}
}
