package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skyhuang233/workflow/internal/codexauth"
	"github.com/skyhuang233/workflow/internal/credential"
	"github.com/skyhuang233/workflow/internal/delivery"
	"github.com/skyhuang233/workflow/internal/doctor"
	"github.com/skyhuang233/workflow/internal/github"
	"github.com/skyhuang233/workflow/internal/plan"
	"github.com/skyhuang233/workflow/internal/startup"
	"github.com/skyhuang233/workflow/internal/store"
	"github.com/skyhuang233/workflow/internal/workflowhome"
)

type githubTokenProviderFunc func(context.Context) (string, error)

func (f githubTokenProviderFunc) Token(ctx context.Context) (string, error) { return f(ctx) }

func TestVerifiedTokenSourceReadsOwnerBoundClassicPAT(t *testing.T) {
	ctx := context.Background()
	layout, err := workflowhome.Resolve(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WORKFLOW_HOME", layout.Root)
	token := "ghp_classic-test"
	if err := credential.NewFileStore(layout.CredentialFile).Set(ctx, credential.GatewayTarget, token); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(ctx, filepath.Join(layout.State, "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.RecordGitHubPATVerification(ctx, store.GitHubPATVerification{FingerprintSHA256: credential.Fingerprint(token), Login: "user", UserID: 7, Owner: "owner", Scopes: []string{"repo", "workflow"}, CredentialPath: layout.CredentialFile, Status: "verified", VerifiedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	config := doctor.Config{SchemaVersion: 6, GitHub: doctor.GitHubPin{Credential: doctor.GitHubCredentialPin{Kind: "classic-pat", Owner: "owner", PlaintextRelativePath: `state\credentials\github.pat`}}}
	got, err := (&verifiedGitHubPATSource{Database: db, Config: config}).Token(ctx)
	if err != nil || got != token {
		t.Fatalf("Token = %q, %v", got, err)
	}
	config.GitHub.Credential.Owner = "different"
	if _, err := (&verifiedGitHubPATSource{Database: db, Config: config}).Token(ctx); !errors.Is(err, delivery.ErrGatewayCredentialRejected) {
		t.Fatalf("owner drift = %v", err)
	}
}

func TestVerifiedClassicPATUsesExplicitCustomWorkflowHomeCredential(t *testing.T) {
	ctx := context.Background()
	layout, err := workflowhome.Resolve(filepath.Join(t.TempDir(), "custom-home"))
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WORKFLOW_HOME", filepath.Join(t.TempDir(), "wrong-default-home"))
	token := "ghp_custom-home"
	if err := credential.NewFileStore(layout.CredentialFile).Set(ctx, credential.GatewayTarget, token); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(ctx, filepath.Join(layout.State, "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.RecordGitHubPATVerification(ctx, store.GitHubPATVerification{FingerprintSHA256: credential.Fingerprint(token), Login: "user", UserID: 7, Owner: "owner", Scopes: []string{"repo", "workflow"}, CredentialPath: layout.CredentialFile, Status: "verified", VerifiedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	config := doctor.Config{SchemaVersion: 6, GitHub: doctor.GitHubPin{Credential: doctor.GitHubCredentialPin{Kind: "classic-pat", Owner: "owner", PlaintextRelativePath: `state\credentials\github.pat`}}}
	got, err := verifiedClassicPAT(ctx, db, config, layout.CredentialFile)
	if err != nil || got != token {
		t.Fatalf("custom Workflow Home token = %q, %v", got, err)
	}
}

type fakeWorkflowInboxAnswerStore struct {
	target       store.TicketClaim
	answerCalls  int
	fenceCalls   int
	acknowledged bool
}

func (f *fakeWorkflowInboxAnswerStore) AnswerWorkflowQuestionAndQueueInboxProjection(_ context.Context, _, _, _ string, _ time.Time, isolated ...store.WorkerIsolationProof) (store.DeliveryOutbox, error) {
	f.answerCalls++
	if f.answerCalls == 1 {
		return store.DeliveryOutbox{}, &store.WorkerIsolationRequired{Targets: []store.TicketClaim{f.target}}
	}
	if len(isolated) != 1 || !f.acknowledged {
		return store.DeliveryOutbox{}, errors.New("answer replayed without acknowledged isolation")
	}
	return store.DeliveryOutbox{}, nil
}

func (f *fakeWorkflowInboxAnswerStore) FenceWorkerIsolation(_ context.Context, targets []store.TicketClaim) ([]store.TicketClaim, error) {
	f.fenceCalls++
	return targets, nil
}

func (f *fakeWorkflowInboxAnswerStore) AcknowledgeWorkerIsolation(_ context.Context, targets []store.TicketClaim) ([]store.WorkerIsolationProof, error) {
	if len(targets) != 1 || targets[0].RunID != f.target.RunID {
		return nil, errors.New("unexpected isolation target")
	}
	f.acknowledged = true
	return []store.WorkerIsolationProof{{}}, nil
}

type restoreContainerIsolatorFunc func(context.Context, string) error

func (f restoreContainerIsolatorFunc) IsolateContainer(ctx context.Context, runID string) error {
	return f(ctx, runID)
}

func (restoreContainerIsolatorFunc) IsolateControlPlaneContainers(context.Context) error {
	return nil
}

type fakeRestoreContainerIsolator struct {
	isolateRun          func(context.Context, string) error
	isolateControlPlane func(context.Context) error
}

func (f fakeRestoreContainerIsolator) IsolateContainer(ctx context.Context, runID string) error {
	return f.isolateRun(ctx, runID)
}

func (f fakeRestoreContainerIsolator) IsolateControlPlaneContainers(ctx context.Context) error {
	return f.isolateControlPlane(ctx)
}

func TestAnswerInboxDefaultsToWorkflowHomeDatabase(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	got, err := answerInboxDatabasePath("", home)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "state", defaultControlPlaneDatabase)
	if got != want {
		t.Fatalf("answer-inbox database = %q, want %q", got, want)
	}
	if _, err := answerInboxDatabasePath("relative.db", home); err == nil {
		t.Fatal("relative advanced database override was accepted")
	}
}

func TestAnswerWorkflowInboxQuestionIsolatesDeliveryControllerBeforeReplay(t *testing.T) {
	ctx := context.Background()
	target := store.TicketClaim{RunID: "delivery-run"}
	db := &fakeWorkflowInboxAnswerStore{target: target}
	var isolated []string
	err := answerWorkflowInboxQuestion(ctx, db, restoreContainerIsolatorFunc(func(_ context.Context, runID string) error {
		isolated = append(isolated, runID)
		return nil
	}), "owner/repository", "question-1", `{"action":"cancel-plan"}`, time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if db.answerCalls != 2 || db.fenceCalls != 1 || !db.acknowledged {
		t.Fatalf("answer isolation handshake = answers %d, fences %d, acknowledged %t", db.answerCalls, db.fenceCalls, db.acknowledged)
	}
	if len(isolated) != 1 || isolated[0] != target.RunID {
		t.Fatalf("isolated Delivery Controllers = %#v", isolated)
	}
}

func TestReconcileRestoredControlPlaneIsolatesPreparedDeliveryBeforeApplying(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	snapshot := plan.Snapshot{
		Repository: "owner/repository",
		Root:       plan.Issue{ID: 100, Number: 100, Labels: []string{plan.PlanLabel}},
		Children:   []plan.Issue{{ID: 1, Number: 1, Title: "ticket", Labels: []string{plan.TicketLabel}, State: "open"}},
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
	claim, err := db.ClaimReady(ctx, store.ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent", MaxParallelRuns: 1, LeaseTTL: time.Hour, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := db.AcceptCandidateForDelivery(ctx, store.CandidateRevision{RunID: claim.RunID, LeaseToken: claim.LeaseToken, CodexSessionID: "codex", CommitSHA: "accepted", StructuredOutput: []byte(`{"summary":"candidate","checks":[{"command":"go test","outcome":"passed"}]}`), Now: now, Publication: store.CandidatePublication{Repository: snapshot.Repository, Branch: "ticket-1", ExpectRemoteAbsent: true, Title: "ticket"}}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ReserveDeliveryControllerPrelaunch(ctx, delivery, now); err != nil {
		t.Fatal(err)
	}
	var isolated []string
	err = reconcileRestoredControlPlane(ctx, db, restoreContainerIsolatorFunc(func(_ context.Context, runID string) error {
		isolated = append(isolated, runID)
		return nil
	}), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(isolated) != 1 || isolated[0] != delivery.RunID {
		t.Fatalf("restored Delivery Controller isolation = %#v", isolated)
	}
	projection, err := db.PlanProjectionAt(ctx, version.ID, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if projection.Tickets[0].State != "Needs Attention" {
		t.Fatalf("restored Delivery Controller state = %q", projection.Tickets[0].State)
	}
}

func TestRestoreValidatesStagedBackupBeforeCurrentIsolation(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	databasePath := filepath.Join(t.TempDir(), "workflow.db")
	db, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	snapshot := plan.Snapshot{Repository: "owner/repository", Root: plan.Issue{ID: 100, Number: 100, Labels: []string{plan.PlanLabel}}, Children: []plan.Issue{{ID: 1, Number: 1, Title: "ticket", Labels: []string{plan.TicketLabel}, State: "open"}}}
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
	claim, err := db.ClaimReady(ctx, store.ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent", MaxParallelRuns: 1, LeaseTTL: time.Hour, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := db.AcceptCandidateForDelivery(ctx, store.CandidateRevision{RunID: claim.RunID, LeaseToken: claim.LeaseToken, CodexSessionID: "codex", CommitSHA: "accepted", StructuredOutput: []byte(`{"summary":"candidate","checks":[{"command":"go test","outcome":"passed"}]}`), Now: now, Publication: store.CandidatePublication{Repository: snapshot.Repository, Branch: "ticket-1", ExpectRemoteAbsent: true, Title: "ticket"}}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ReserveDeliveryControllerPrelaunch(ctx, delivery, now); err != nil {
		t.Fatal(err)
	}
	var isolated []string
	err = restoreControlPlane(ctx, filepath.Join(t.TempDir(), "missing.backup"), databasePath, restoreContainerIsolatorFunc(func(_ context.Context, runID string) error {
		isolated = append(isolated, runID)
		return nil
	}), now)
	if err == nil {
		t.Fatal("invalid backup passed staged validation")
	}
	if len(isolated) != 0 {
		t.Fatalf("current control plane isolated before staged validation: %#v", isolated)
	}
}

func TestRestoreReplacesCorruptCurrentDatabase(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	directory := t.TempDir()
	sourcePath := filepath.Join(directory, "source.db")
	source, err := store.Open(ctx, sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	backupPath := filepath.Join(directory, "workflow.backup")
	if _, err := source.CreateOnlineBackup(ctx, backupPath, now); err != nil {
		source.Close()
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(directory, "workflow.db")
	if err := os.WriteFile(databasePath, []byte("not a sqlite database"), 0o600); err != nil {
		t.Fatal(err)
	}
	contained := false
	isolator := fakeRestoreContainerIsolator{
		isolateRun: func(context.Context, string) error { return nil },
		isolateControlPlane: func(context.Context) error {
			contained = true
			return nil
		},
	}
	if err := restoreControlPlane(ctx, backupPath, databasePath, isolator, now); err != nil {
		t.Fatal(err)
	}
	if !contained {
		t.Fatal("corrupt current Control Plane was replaced without database-independent container isolation")
	}
	restored, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatalf("open restored database: %v", err)
	}
	defer restored.Close()
	if _, err := restored.ReconcilePreview(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreFencedGatewayDrainsAndReopensPublishedDatabase(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	databasePath := filepath.Join(directory, "workflow.db")
	db, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := plan.Snapshot{
		Repository: "owner/repository",
		Root:       plan.Issue{ID: 100, Number: 100, Labels: []string{plan.PlanLabel}},
		Children:   []plan.Issue{{ID: 1, Number: 1, Title: "ticket", Labels: []string{plan.TicketLabel}, State: "open"}},
	}
	fingerprint, err := snapshot.Fingerprint()
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	version, err := db.BeginActivation(ctx, snapshot, fingerprint, "revision-1")
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.MarkActive(ctx, version.ID); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	replacementPath := filepath.Join(directory, "replacement.db")
	replacement, err := store.Open(ctx, replacementPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := replacement.Close(); err != nil {
		t.Fatal(err)
	}
	gateway, err := newRestoreFencedGateway(databasePath, doctor.Config{}, "https://api.github.com", "")
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	gatewayDone := make(chan error, 1)
	go func() {
		gatewayDone <- gateway.withGateway(ctx, func(*store.Store, delivery.Gateway, func(context.Context) (string, error)) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	type barrierResult struct {
		lock *startup.Lock
		err  error
	}
	barrierDone := make(chan barrierResult, 1)
	go func() {
		lock, err := startup.AcquireRestoreBarrier(ctx, databasePath)
		barrierDone <- barrierResult{lock: lock, err: err}
	}()
	select {
	case result := <-barrierDone:
		if result.lock != nil {
			result.lock.Close()
		}
		t.Fatalf("restore crossed active Gateway database access: %v", result.err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-gatewayDone; err != nil {
		t.Fatal(err)
	}
	result := <-barrierDone
	if result.err != nil {
		t.Fatal(result.err)
	}
	if err := store.PublishRestoredBackup(ctx, replacementPath, databasePath); err != nil {
		result.lock.Close()
		t.Fatal(err)
	}
	if err := result.lock.Close(); err != nil {
		t.Fatal(err)
	}
	err = gateway.withGateway(ctx, func(db *store.Store, _ delivery.Gateway, _ func(context.Context) (string, error)) error {
		_, err := db.PlanProjectionAt(ctx, version.ID, time.Now().UTC())
		return err
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Gateway retained pre-restore database = %v", err)
	}
}

func TestDefaultCodexAuthFileRequiresExplicitWorkflowIntegrationSource(t *testing.T) {
	home := filepath.Join(t.TempDir(), "codex-home")
	t.Setenv("CODEX_HOME", home)
	t.Setenv(codexauth.SourceOverrideEnvironment, "")
	if got := defaultCodexAuthFile(); got != "" {
		t.Fatalf("defaultCodexAuthFile() guessed private source %q", got)
	}
	want := filepath.Join(t.TempDir(), "supported-source.json")
	t.Setenv(codexauth.SourceOverrideEnvironment, want)
	if got := defaultCodexAuthFile(); got != want {
		t.Fatalf("defaultCodexAuthFile() = %q, want %q", got, want)
	}
}

func TestResolveDoctorCodexAuthUsesVerifiedDoctorSource(t *testing.T) {
	want := filepath.Join(t.TempDir(), "auth.json")
	got, err := resolveDoctorCodexAuth(context.Background(), "", func(context.Context) (string, error) {
		return want, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("resolved source = %q, want %q", got, want)
	}
}

func TestResolveDoctorCodexAuthRejectsUnverifiedRequestedSource(t *testing.T) {
	verified := filepath.Join(t.TempDir(), "verified", "auth.json")
	requested := filepath.Join(t.TempDir(), "requested", "auth.json")
	_, err := resolveDoctorCodexAuth(context.Background(), requested, func(context.Context) (string, error) {
		return verified, nil
	})
	if err == nil || !strings.Contains(err.Error(), "must match") {
		t.Fatalf("unverified requested source error = %v", err)
	}
}

func TestDoctorVerificationBudgetAllowsColdWorkerPullAndCodexResume(t *testing.T) {
	if doctorVerificationTimeout != 10*time.Minute {
		t.Fatalf("doctorVerificationTimeout = %s, want 10m", doctorVerificationTimeout)
	}
	t.Logf("workflow doctor verification budget = %s", doctorVerificationTimeout)
}

func TestImplementationPromptCarriesPersistedTicketContract(t *testing.T) {
	claim := store.TicketClaim{TicketNumber: 8, TicketTitle: "Add the alpha record"}
	body := "Create qualification/issue20-e2e.md with exactly:\nalpha: issue-20-production-e2e"
	prompt := implementationPrompt(claim, body)
	for _, want := range []string{
		"Implement Executable Ticket #8: Add the alpha record",
		body,
		"Do not call GitHub",
		"Commit all changes and leave the Ticket Workspace clean.",
		"exact full lowercase 40-character Git commit SHA",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("implementationPrompt() omitted %q:\n%s", want, prompt)
		}
	}
}

func TestResolveWorkerPromptUsesImmutableBodyForInitialRun(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	snapshot := plan.Snapshot{
		Repository: "owner/repo",
		Root:       plan.Issue{ID: 100, Number: 10, Labels: []string{plan.PlanLabel}},
		Children:   []plan.Issue{{ID: 1, Number: 11, Title: "first", Body: "authoritative acceptance criteria", Labels: []string{plan.TicketLabel}, State: "open"}},
	}
	fingerprint, err := snapshot.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	version, err := db.BeginActivation(ctx, snapshot, fingerprint, "revision-1")
	if err != nil {
		t.Fatal(err)
	}
	claim := store.TicketClaim{VersionID: version.ID, TicketID: 1, TicketNumber: 11, TicketTitle: "first"}
	prompt, err := resolveWorkerPrompt(ctx, db, claim, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, snapshot.Children[0].Body) {
		t.Fatalf("initial Worker prompt = %q", prompt)
	}
}

func TestResolveWorkerPromptPreservesReviewRevisionExactly(t *testing.T) {
	persisted := "  Review feedback ID review/50:\nkeep surrounding whitespace exactly  "
	prompt, err := resolveWorkerPrompt(context.Background(), nil, store.TicketClaim{}, persisted)
	if err != nil {
		t.Fatal(err)
	}
	if prompt != persisted {
		t.Fatalf("review revision prompt = %q, want %q", prompt, persisted)
	}
}

func TestRecoverInboxDeliveryCLIListsAndAuthorizesOldestGeneration(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "workflow.db")
	db, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	repository := "owner/repository"
	snapshot := plan.Snapshot{
		Repository: repository,
		Root:       plan.Issue{ID: 1, Number: 1, Labels: []string{plan.PlanLabel}},
		Children:   []plan.Issue{{ID: 2, Number: 2, Labels: []string{plan.TicketLabel}, State: "open"}},
	}
	fingerprint, err := snapshot.Fingerprint()
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	version, err := db.BeginActivation(ctx, snapshot, fingerprint, "revision-1")
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.MarkActive(ctx, version.ID); err != nil {
		db.Close()
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Hour)
	queued, err := db.QueueWorkflowInboxProjection(ctx, repository, now)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	deliveryKey := queued.IdempotencyKey
	for attempt := 0; attempt < 8; attempt++ {
		claim, claimErr := db.ClaimDeliveryOutbox(ctx, deliveryKey, now)
		if claimErr != nil {
			db.Close()
			t.Fatalf("claim attempt %d: %v", attempt+1, claimErr)
		}
		if err := db.RequeueDeliveryOutboxClaim(ctx, deliveryKey, claim.ClaimToken, "remote observation unavailable", true, now); err != nil {
			db.Close()
			t.Fatal(err)
		}
		outbox, err := db.DeliveryOutbox(ctx, deliveryKey)
		if err != nil {
			db.Close()
			t.Fatal(err)
		}
		if outbox.State == store.OutboxRejected {
			break
		}
		now = now.Add(time.Minute)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	binaryPath := filepath.Join(t.TempDir(), "workflow-test.exe")
	build := exec.Command("go", "build", "-o", binaryPath, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build workflow CLI: %v\n%s", err, output)
	}
	list := exec.Command(binaryPath, "recover-inbox-delivery", "--database", databasePath, "--repository", repository)
	output, err := list.CombinedOutput()
	if err != nil {
		t.Fatalf("list recoverable Inbox deliveries: %v\n%s", err, output)
	}
	fields := strings.Fields(string(output))
	if len(fields) != 2 || fields[0] != deliveryKey {
		t.Fatalf("recovery listing = %q, want delivery key and question id", output)
	}
	questionID := fields[1]
	t.Logf("$ workflow recover-inbox-delivery --repository %s", repository)
	t.Logf("%s %s", deliveryKey, questionID)

	recoverCommand := exec.Command(binaryPath, "recover-inbox-delivery", "--database", databasePath, "--repository", repository, "--delivery", deliveryKey, "--question", questionID, "--answer", "retry")
	if output, err := recoverCommand.CombinedOutput(); err != nil {
		t.Fatalf("authorize Inbox recovery: %v\n%s", err, output)
	}
	t.Logf("$ workflow recover-inbox-delivery --repository %s --delivery %s --question %s --answer retry", repository, deliveryKey, questionID)

	db, err = store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	recovered, err := db.DeliveryOutbox(ctx, deliveryKey)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.State != store.OutboxPending || !recovered.Uncertain || recovered.Attempts != 0 {
		t.Fatalf("authorized recovery = %#v", recovered)
	}
	keys, err := db.DueDeliveryOutboxKeys(ctx, time.Now().UTC().Add(time.Minute), 10)
	if err != nil || len(keys) < 2 || keys[0] != deliveryKey {
		t.Fatalf("ordered recovery queue = %v, %v", keys, err)
	}
	currentGenerations := make([]int64, 0, len(keys)-1)
	for _, key := range keys[1:] {
		current, err := db.DeliveryOutbox(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if current.Request.InboxProjectionGeneration <= recovered.Request.InboxProjectionGeneration {
			t.Fatalf("ordered generations = %d then %d", recovered.Request.InboxProjectionGeneration, current.Request.InboxProjectionGeneration)
		}
		currentGenerations = append(currentGenerations, current.Request.InboxProjectionGeneration)
	}
	t.Logf("authorized generation %d returned to the queue before current generations %v", recovered.Request.InboxProjectionGeneration, currentGenerations)
}

func TestShouldPauseGatewayForCredential(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "missing", err: fmt.Errorf("%w: PAT file is missing", delivery.ErrGatewayCredentialRejected), want: true},
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

func TestGitHubCredentialAdmissionsNormalizeAndPersistRejectedPAT(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rejected := &github.APIError{StatusCode: http.StatusUnauthorized}
	provider := githubTokenProviderFunc(func(context.Context) (string, error) { return "", rejected })
	_, err = admitPollGitHubCredential(ctx, github.Poller{Store: db}, provider, "owner/repo", nil)
	if !errors.Is(err, delivery.ErrGatewayCredentialRejected) {
		t.Fatalf("poll-github rejected PAT error = %v", err)
	}
	_, err = admitControlPlaneGitHubCredential(ctx, db, provider, nil)
	if !errors.Is(err, delivery.ErrGatewayCredentialRejected) {
		t.Fatalf("Gateway rejected PAT error = %v", err)
	}
	paused, _, pauseErr := db.GatewayWritesPaused(ctx)
	if pauseErr != nil || !paused {
		t.Fatalf("Gateway writes paused = %t, %v", paused, pauseErr)
	}
}

func TestShouldLogNeedsAttentionErrorWithInboxRecoveryCommand(t *testing.T) {
	plain := fmt.Errorf("poll exhausted: %w", store.ErrNeedsAttention)
	if shouldLogNeedsAttentionError(plain) {
		t.Fatal("plain Needs Attention error should remain suppressed")
	}
	actionable := errors.Join(plain, errors.New("workflow recover-inbox-delivery --repository owner/repo --delivery inbox-key"))
	if !shouldLogNeedsAttentionError(actionable) {
		t.Fatal("uncertain Inbox recovery command should be logged")
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

func TestGatewayControlProjectorSendsHostInboxProjectionToOverride(t *testing.T) {
	const controlToken = "control-token"
	requests := 0
	controlGateway := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		if request.URL.Path != "/v1/deliveries" {
			t.Errorf("control projection path = %q, want /v1/deliveries", request.URL.Path)
		}
		if got := request.Header.Get("X-Workflow-Control-Token"); got != controlToken {
			t.Errorf("control projection token = %q, want %q", got, controlToken)
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer controlGateway.Close()

	projector := gatewayControlProjector("http://host.docker.internal:8787", controlGateway.URL, controlToken)
	if err := projector.ProjectWorkflowInbox(context.Background(), "owner/repo", nil); err != nil {
		t.Fatalf("project host inbox through control Gateway: %v", err)
	}
	if requests != 1 {
		t.Fatalf("control Gateway inbox requests = %d, want 1", requests)
	}
	t.Logf("Inbox projection reached the host control Gateway at %s while Worker routing remains %s", controlGateway.URL, "http://host.docker.internal:8787")
}

func TestPersistGitHubCredentialPauseCreatesLocalRecoveryInbox(t *testing.T) {
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
	credentialErr := fmt.Errorf("%w: PAT file is missing", delivery.ErrGatewayCredentialRejected)
	if err := persistGitHubCredentialPause(ctx, db, credentialErr, now); !errors.Is(err, delivery.ErrGatewayCredentialRejected) {
		t.Fatalf("credential pause error = %v", err)
	}
	paused, _, err := db.GatewayWritesPaused(ctx)
	if err != nil || !paused {
		t.Fatalf("Gateway writes paused = %t, %v", paused, err)
	}
	inbox, err := db.WorkflowInboxItem(ctx, store.GatewayCredentialInboxKey)
	if err != nil || inbox.State != "open" || inbox.Title != store.ControlPlaneGitHubCredentialRecoveryTitle || inbox.Body != store.ControlPlaneGitHubCredentialRecoveryRemediation {
		t.Fatalf("credential recovery inbox = %#v, %v", inbox, err)
	}
	questions, err := db.OpenWorkflowQuestions(ctx, "owner/repo", 10)
	if err != nil || len(questions) != 1 {
		t.Fatalf("credential recovery questions = %#v, %v", questions, err)
	}
}

func TestPersistGitHubCredentialPauseLeavesTransientFailuresRetryable(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	transient := errors.New("PAT verification store temporarily unavailable")
	if err := persistGitHubCredentialPause(ctx, db, transient, time.Now().UTC()); !errors.Is(err, transient) {
		t.Fatalf("transient credential error = %v", err)
	}
	paused, _, err := db.GatewayWritesPaused(ctx)
	if err != nil || paused {
		t.Fatalf("Gateway writes paused = %t, %v", paused, err)
	}
}

func TestPersistGitHubCredentialAdmissionErrorPausesForRejectedCredential(t *testing.T) {
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
	if err := persistGitHubCredentialAdmissionError(ctx, db, pollErr, now); !errors.Is(err, delivery.ErrGatewayCredentialRejected) {
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

func TestPersistGitHubCredentialAdmissionErrorLeavesRateLimitsRetryable(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	retryAt := time.Date(2026, 8, 4, 0, 1, 0, 0, time.UTC)
	pollErr := &github.APIError{StatusCode: http.StatusForbidden, RetryAt: retryAt}
	if err := persistGitHubCredentialAdmissionError(ctx, db, pollErr, time.Now().UTC()); !errors.Is(err, pollErr) {
		t.Fatalf("rate limited poll error = %v", err)
	}
	paused, _, err := db.GatewayWritesPaused(ctx)
	if err != nil || paused {
		t.Fatalf("Gateway writes paused = %t, %v", paused, err)
	}
}

func TestCredentialAdmissionConsumesBootstrapWithoutTerminalizingWorkers(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "missing credential", err: fmt.Errorf("%w: PAT file is missing", delivery.ErrGatewayCredentialRejected)},
		{name: "rejected by GitHub", err: &github.APIError{StatusCode: http.StatusUnauthorized}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			dbPath := filepath.Join(t.TempDir(), "workflow.db")
			db, err := store.Open(ctx, dbPath)
			if err != nil {
				t.Fatal(err)
			}
			repository := "owner/repo"
			now := time.Now().UTC()
			snapshot := plan.Snapshot{
				Repository: repository,
				Root:       plan.Issue{ID: 100, Number: 10, Labels: []string{plan.PlanLabel}},
				Children:   []plan.Issue{{ID: 1, Number: 11, Title: "first", Labels: []string{plan.TicketLabel}, State: "open"}},
			}
			fingerprint, err := snapshot.Fingerprint()
			if err != nil {
				db.Close()
				t.Fatal(err)
			}
			version, err := db.BeginActivation(ctx, snapshot, fingerprint, "revision-1")
			if err != nil {
				db.Close()
				t.Fatal(err)
			}
			if err := db.MarkActive(ctx, version.ID); err != nil {
				db.Close()
				t.Fatal(err)
			}
			claim, err := db.ClaimReady(ctx, store.ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent", MaxParallelRuns: 1, LeaseTTL: time.Hour, Now: now})
			if err != nil {
				db.Close()
				t.Fatal(err)
			}
			if err := db.RecordGitHubPollFailureWithKind(ctx, repository, now, store.GitHubPollFailurePreActivationInboxConflict); err != nil {
				db.Close()
				t.Fatal(err)
			}
			admissionErr := persistGitHubCredentialAdmissionError(ctx, db, test.err, now.Add(time.Second))
			err = recordPollAdmissionFailure(ctx, github.Poller{Store: db, MaxFailures: 5, Now: func() time.Time { return now.Add(time.Second) }}, repository, admissionErr)
			if !errors.Is(err, test.err) && !errors.Is(err, delivery.ErrGatewayCredentialRejected) {
				db.Close()
				t.Fatalf("credential admission failure = %v", err)
			}
			paused, _, pauseErr := db.GatewayWritesPaused(ctx)
			cursor, cursorErr := db.GitHubPollCursor(ctx, repository)
			current, claimErr := db.CurrentClaim(ctx, version.ID, claim.TicketID)
			if pauseErr != nil || !paused || cursorErr != nil || cursor.NeedsAttention() || cursor.ConsecutiveFailures != 0 || cursor.FailureKind != store.GitHubPollFailureRetryable || cursor.RecoveryState != store.GitHubPollRecoveryConsumed || claimErr != nil || current.RunID != claim.RunID {
				db.Close()
				t.Fatalf("credential state paused=%t cursor=%#v claim=%#v errors=%v/%v/%v", paused, cursor, current, pauseErr, cursorErr, claimErr)
			}
			questions, questionErr := db.OpenWorkflowQuestions(ctx, repository, 0)
			if questionErr != nil || len(questions) != 1 || questions[0].Kind != "gateway_credential" {
				db.Close()
				t.Fatalf("credential questions = %#v, %v", questions, questionErr)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			restarted, err := store.Open(ctx, dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			cursor, err = restarted.GitHubPollCursor(ctx, repository)
			if err != nil || cursor.NeedsAttention() || cursor.ConsecutiveFailures != 0 || cursor.FailureKind != store.GitHubPollFailureRetryable || cursor.RecoveryState != store.GitHubPollRecoveryConsumed {
				t.Fatalf("restarted credential cursor = %#v, %v", cursor, err)
			}
		})
	}
}

func TestPollAdmissionHonorsNextAttemptBeforeCredentialAccess(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := "owner/repo"
	now := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)
	if err := db.DeferGitHubPoll(ctx, repository, now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	authenticated := false
	_, err = admitPollGitHubCredential(ctx, github.Poller{Store: db, Now: func() time.Time { return now }}, nil, repository, func(string) error {
		authenticated = true
		return nil
	})
	if !errors.Is(err, store.ErrNotReady) || authenticated {
		t.Fatalf("deferred admission error=%v authenticated=%t", err, authenticated)
	}
	paused, _, pauseErr := db.GatewayWritesPaused(ctx)
	if pauseErr != nil || paused {
		t.Fatalf("deferred admission paused=%t error=%v", paused, pauseErr)
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
	t.Logf("run-ticket claim acquisition reused Ticket Session %s and advanced from attempt %d to %d", replacement.SessionID, expired.Attempt, replacement.Attempt)
}

func TestAcquireTicketClaimRestoresClaimedReviewPromptBeforeControllerRun(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	snapshot := plan.Snapshot{
		Repository: "owner/repo",
		Root:       plan.Issue{ID: 100, Number: 10, Labels: []string{plan.PlanLabel}},
		Children:   []plan.Issue{{ID: 1, Number: 11, Title: "first", Body: "initial specification must not replace review feedback", Labels: []string{plan.TicketLabel}, State: "open"}},
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
	delivery, err := db.AcceptCandidateForDelivery(ctx, store.CandidateRevision{RunID: claim.RunID, LeaseToken: claim.LeaseToken, CodexSessionID: "codex", CommitSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", StructuredOutput: []byte(`{"summary":"candidate","checks":[{"command":"go test","outcome":"passed"}]}`), Now: now, Publication: store.CandidatePublication{Repository: snapshot.Repository, Branch: "ticket-1", ExpectRemoteAbsent: true, Title: "ticket"}}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteDeliveryController(ctx, delivery, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	feedbackBody := "  preserve this review feedback exactly  \nincluding its whitespace"
	if _, err := db.RecordReviewFeedback(ctx, version.ID, claim.TicketID, []store.ReviewFeedback{{Source: "review", EventID: "50", Author: "human", Body: feedbackBody}}, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	revision, expectedPrompt, err := db.ClaimQueuedReviewRevision(ctx, version.ID, claim.TicketID, time.Hour, now.Add(3*time.Second), 1, store.DefaultMaxWorkerAttempts)
	if err != nil {
		t.Fatal(err)
	}
	recovered, recoveredPrompt, err := acquireTicketClaim(ctx, db, version.ID, claim.TicketID, store.DefaultMaxWorkerAttempts, now.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if recovered.RunID != revision.RunID || recoveredPrompt != expectedPrompt {
		t.Fatalf("recovered claim = %#v, prompt = %q; want run %q, prompt %q", recovered, recoveredPrompt, revision.RunID, expectedPrompt)
	}
	resolved, err := resolveWorkerPrompt(ctx, db, recovered, recoveredPrompt)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != expectedPrompt || strings.Contains(resolved, snapshot.Children[0].Body) {
		t.Fatalf("resolved recovery prompt = %q, want exact claimed review prompt %q", resolved, expectedPrompt)
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
