package setup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skyhuang233/workflow/internal/credential"
	workflowgithub "github.com/skyhuang233/workflow/internal/github"
	"github.com/skyhuang233/workflow/internal/githubcredential"
	"github.com/skyhuang233/workflow/internal/onboarding"
	"github.com/skyhuang233/workflow/internal/platformrelease"
	"github.com/skyhuang233/workflow/internal/setupcontract"
	"github.com/skyhuang233/workflow/internal/store"
	"github.com/skyhuang233/workflow/internal/workflowhome"
)

type fakeAdapter struct {
	states   map[string]setupcontract.EffectStatus
	evidence map[string]string
	applied  []string
	read     []string
	restored []setupcontract.EffectResult
	fail     string
}

type cleanupRetryAdapter struct {
	*fakeAdapter
	failCleanup bool
	reconciles  int
}

func (a *cleanupRetryAdapter) CleanupObligations(effect setupcontract.Effect, _ string) ([]store.SetupCleanupObligation, error) {
	if effect.ID != "first" {
		return nil, nil
	}
	return []store.SetupCleanupObligation{{ObligationID: "first:temporary", Kind: "temporary_clone", Resource: `{"root":"C:/tmp","prefix":"C:/tmp/workflow-onboarding-a-"}`}}, nil
}

func (a *cleanupRetryAdapter) ReconcileCleanupObligation(context.Context, store.SetupCleanupObligation) error {
	a.reconciles++
	if a.failCleanup {
		a.failCleanup = false
		return errors.New("injected cleanup failure")
	}
	return nil
}

func (a *cleanupRetryAdapter) ValidateCleanupObligation(setupcontract.Plan, store.SetupCleanupObligation) error {
	return nil
}

type repositoryCreateAttemptAdapter struct {
	*fakeAdapter
	databasePath string
	planID       string
	digest       string
	repository   string
	private      bool
	created      bool
	startedSeen  bool
	applyErr     error
	applyCalls   int
}

func (a *repositoryCreateAttemptAdapter) Readback(ctx context.Context, effect setupcontract.Effect) (setupcontract.EffectStatus, string, error) {
	if effect.Kind != "create_repository" {
		return a.fakeAdapter.Readback(ctx, effect)
	}
	if !a.created {
		return setupcontract.EffectRequired, "absent", nil
	}
	if a.states[effect.ID] != setupcontract.EffectSatisfied {
		return setupcontract.EffectConflicting, "appeared without attempt evidence", nil
	}
	return setupcontract.EffectSatisfied, repositoryCreatedEvidence + effect.ID + ":" + effect.Subject, nil
}

func (a *repositoryCreateAttemptAdapter) Apply(ctx context.Context, effect setupcontract.Effect, input *SecretInput) error {
	if effect.Kind != "create_repository" {
		return a.fakeAdapter.Apply(ctx, effect, input)
	}
	db, err := store.OpenReadOnly(ctx, a.databasePath)
	if err != nil {
		return err
	}
	events, readErr := db.SetupRepositoryCreateAttemptEvents(ctx, a.planID, effect.ID)
	closeErr := db.Close()
	if readErr != nil || closeErr != nil {
		return errors.Join(readErr, closeErr)
	}
	a.startedSeen = len(events) == 1 && events[0].Event == store.RepositoryCreateStarted && events[0].PlanDigestSHA256 == a.digest && events[0].ApprovalAbsentRepository == a.repository && events[0].Private == a.private
	a.applyCalls++
	if a.applyErr != nil {
		return a.applyErr
	}
	a.created = true
	a.states[effect.ID] = setupcontract.EffectSatisfied
	return nil
}

func (a *repositoryCreateAttemptAdapter) RestoreRepositoryCreateAttemptEvents(effect setupcontract.Effect, events []store.SetupRepositoryCreateAttemptEvent) error {
	for _, event := range events {
		if event.EffectID == effect.ID && event.PlanDigestSHA256 == a.digest && event.ApprovalAbsentRepository == a.repository && event.Private == a.private && event.Event != store.RepositoryCreateDefinitiveFailure {
			a.states[event.EffectID] = setupcontract.EffectSatisfied
		}
	}
	return nil
}

func (*repositoryCreateAttemptAdapter) RepositoryCreateOutcomeUnknown(error) bool { return true }

func (f *fakeAdapter) Readback(_ context.Context, e setupcontract.Effect) (setupcontract.EffectStatus, string, error) {
	f.read = append(f.read, e.ID)
	state := f.states[e.ID]
	if state == "" {
		state = setupcontract.EffectRequired
	}
	evidence := string(state)
	if f.evidence[e.ID] != "" {
		evidence = f.evidence[e.ID]
	}
	return state, evidence, nil
}

func (f *fakeAdapter) RestoreEffectResults(results []setupcontract.EffectResult) error {
	f.restored = append(f.restored, results...)
	return nil
}

func (f *fakeAdapter) CheckPrecondition(_ context.Context, p setupcontract.Precondition) error {
	if p.Expected == "drifted" {
		return errors.New("precondition drifted")
	}
	return nil
}

func (f *fakeAdapter) CheckPreLayoutPrecondition(ctx context.Context, p setupcontract.Precondition) error {
	return f.CheckPrecondition(ctx, p)
}
func (f *fakeAdapter) Apply(_ context.Context, e setupcontract.Effect, _ *SecretInput) error {
	if e.ID == f.fail {
		return errors.New("injected failure")
	}
	f.applied = append(f.applied, e.ID)
	f.states[e.ID] = setupcontract.EffectSatisfied
	return nil
}

func TestEngineAppliesRequiredEffectsAndRetriesSatisfiedOnes(t *testing.T) {
	home := filepath.Join(t.TempDir(), "WorkflowHome")
	plan := testPlan(home)
	raw, _ := json.Marshal(plan)
	_, _, digest, err := setupcontract.ParsePlan(raw)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &fakeAdapter{states: map[string]setupcontract.EffectStatus{}, fail: "second"}
	engine := Engine{Adapter: adapter, SecretInput: &SecretInput{Reader: bytes.NewBufferString("secret\n")}, ExpectedResultVerifier: passingExpectedResultVerifier, PlatformPreconditionVerifier: passingPlatformPreconditionVerifier}
	first, err := engine.Apply(context.Background(), raw, digest)
	if err == nil || first.Status != setupcontract.ExecutionIncomplete {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	adapter.fail = ""
	second, err := engine.Apply(context.Background(), raw, digest)
	if err != nil || second.Status != setupcontract.ExecutionSucceeded {
		t.Fatalf("second=%#v err=%v", second, err)
	}
	if len(adapter.applied) != 2 || adapter.applied[0] != "first" || adapter.applied[1] != "second" {
		t.Fatalf("applied=%v", adapter.applied)
	}
}

func TestEngineRetriesPendingCleanupBeforeSatisfiedEffectReadback(t *testing.T) {
	home := filepath.Join(t.TempDir(), "WorkflowHome")
	plan := testPlan(home)
	raw, _ := json.Marshal(plan)
	_, _, digest, err := setupcontract.ParsePlan(raw)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &cleanupRetryAdapter{fakeAdapter: &fakeAdapter{states: map[string]setupcontract.EffectStatus{}}, failCleanup: true}
	engine := Engine{Adapter: adapter, ExpectedResultVerifier: passingExpectedResultVerifier, PlatformPreconditionVerifier: passingPlatformPreconditionVerifier}
	first, err := engine.Apply(context.Background(), raw, digest)
	if err == nil || first.Status != setupcontract.ExecutionIncomplete || !strings.Contains(err.Error(), "cleanup") || len(adapter.applied) != 1 {
		t.Fatalf("first=%#v err=%v applied=%v", first, err, adapter.applied)
	}
	second, err := engine.Apply(context.Background(), raw, digest)
	if err != nil || second.Status != setupcontract.ExecutionSucceeded || len(adapter.applied) != 2 || adapter.applied[0] != "first" || adapter.applied[1] != "second" || adapter.reconciles < 2 {
		t.Fatalf("second=%#v err=%v applied=%v reconciles=%d", second, err, adapter.applied, adapter.reconciles)
	}
}

func TestReplacementPlanDrainsPendingCleanupFromEarlierTrustedPlan(t *testing.T) {
	home := filepath.Join(t.TempDir(), "WorkflowHome")
	layout, err := workflowhome.Resolve(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	planA := testPlan(home)
	planA.PlanID = "plan-a"
	rawA, _ := json.Marshal(planA)
	_, canonicalA, digestA, err := setupcontract.ParsePlan(rawA)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(context.Background(), filepath.Join(layout.State, "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RecordSetupPlan(context.Background(), store.SetupPlanRecord{PlanID: planA.PlanID, Kind: string(planA.Kind), SchemaVersion: planA.SchemaVersion, Target: home, DigestSHA256: digestA, CanonicalJSON: string(canonicalA), Projection: Project(planA, digestA), CreatedAt: testTime()}); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordSetupCleanupObligation(context.Background(), store.SetupCleanupObligation{PlanID: planA.PlanID, PlanDigestSHA256: digestA, EffectID: "first", ObligationID: "first:temporary", Kind: "temporary_clone", Resource: `{"root":"C:/tmp","path":"C:/tmp/workflow-onboarding-aaaaaaaaaaaa"}`, Status: store.CleanupPending, UpdatedAt: testTime()}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	planB := testPlan(home)
	planB.PlanID = "plan-b"
	rawB, _ := json.Marshal(planB)
	_, _, digestB, err := setupcontract.ParsePlan(rawB)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &cleanupRetryAdapter{fakeAdapter: &fakeAdapter{states: map[string]setupcontract.EffectStatus{"first": setupcontract.EffectSatisfied, "second": setupcontract.EffectSatisfied}}}
	result, err := (&Engine{Adapter: adapter, ExpectedResultVerifier: passingExpectedResultVerifier, PlatformPreconditionVerifier: passingPlatformPreconditionVerifier}).Apply(context.Background(), rawB, digestB)
	if err != nil || result.Status != setupcontract.ExecutionSucceeded || adapter.reconciles != 1 {
		t.Fatalf("result=%#v err=%v reconciles=%d", result, err, adapter.reconciles)
	}
	db, err = store.OpenReadOnly(context.Background(), filepath.Join(layout.State, "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	pending, err := db.PendingSetupCleanupObligationsAll(context.Background())
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending=%#v err=%v", pending, err)
	}
}

func TestApprovedPATReplacementPreflightsNewCredentialForEarlierRemoteCleanup(t *testing.T) {
	layout, err := workflowhome.Resolve(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	contractDigest := writeTestPlatformSetupContract(t, layout)
	const repository = "owner/repo"
	branch := ""
	head := strings.Repeat("a", 40)
	planA := setupcontract.Plan{
		SchemaVersion: 1, PlanID: "cleanup-plan-a", Kind: setupcontract.RepositoryOnboarding,
		Target:        setupcontract.Target{WorkflowHome: layout.Root, RepositoryPath: filepath.Join(t.TempDir(), "repo"), GitHubRepository: repository},
		Preconditions: []setupcontract.Precondition{{ID: "head", Kind: "git_head", Subject: filepath.Join(t.TempDir(), "repo"), Expected: head}},
		Effects: []setupcontract.Effect{
			{ID: "contract", Kind: "repository_contract_pr", Subject: repository, Action: "create_check_merge", Parameters: map[string]string{"base_branch": "main", "base_head": head, "source_url": "https://github.com/owner/repo.git", "before_files_json": `{}`, "files_json": `{}`, "manifest_digest": repeat("b", 64), "required_checks_json": `[{"context":"workflow-contract","app_id":15368}]`}},
			{ID: "admit", Kind: "repository_admission", Subject: repository, Action: "verify_and_record", Parameters: map[string]string{"default_branch": "main", "manifest_digest": repeat("b", 64), "contract_version": "1", "labels_json": `[]`, "actions_allowed": "all"}},
		},
		ExpectedResults: []setupcontract.ExpectedResult{{ID: "ready", Kind: "repository_admission", Subject: repository, Expected: repeat("b", 64)}},
	}
	rawA, _ := json.Marshal(planA)
	_, canonicalA, digestA, err := setupcontract.ParsePlan(rawA)
	if err != nil {
		t.Fatal(err)
	}
	branch = "workflow/onboarding-" + digestA[:12]
	database, err := store.Open(context.Background(), filepath.Join(layout.State, "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := database.RecordSetupPlan(context.Background(), store.SetupPlanRecord{PlanID: planA.PlanID, Kind: string(planA.Kind), SchemaVersion: planA.SchemaVersion, Target: repository, DigestSHA256: digestA, CanonicalJSON: string(canonicalA), Projection: Project(planA, digestA), CreatedAt: testTime()}); err != nil {
		t.Fatal(err)
	}
	resource, _ := json.Marshal(remoteBranchCleanupResource{Repository: repository, Branch: branch, Head: head})
	if err := database.RecordSetupCleanupObligation(context.Background(), store.SetupCleanupObligation{PlanID: planA.PlanID, PlanDigestSHA256: digestA, EffectID: "contract", ObligationID: "contract:remote-branch", Kind: "remote_onboarding_branch", Resource: string(resource), Status: store.CleanupPending, UpdatedAt: testTime()}); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if err := credential.NewFileStore(layout.CredentialFile).Set(context.Background(), credential.GatewayTarget, "ghp_revoked"); err != nil {
		t.Fatal(err)
	}
	deletes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer ghp_replacement" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/user":
			w.Header().Set("X-OAuth-Scopes", "repo, workflow")
			_, _ = w.Write([]byte(`{"login":"owner","id":7}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/git/ref/heads/"):
			_, _ = fmt.Fprintf(w, `{"object":{"sha":"%s"}}`, head)
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/git/refs/heads/"):
			deletes++
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	planB := setupcontract.Plan{
		SchemaVersion: 1, PlanID: "replace-pat-and-clean", Kind: setupcontract.PlatformBootstrap, Target: setupcontract.Target{WorkflowHome: layout.Root},
		Preconditions:   []setupcontract.Precondition{{ID: "release", Kind: "platform_release", Subject: "v1", Expected: repeat("a", 64)}},
		Effects:         []setupcontract.Effect{{ID: "pat", Kind: "github_pat", Subject: layout.CredentialFile, Action: "replace", Parameters: map[string]string{"input": "stdin", "owner": "owner", "required_scopes": "repo,workflow", "fingerprint_sha256": patFingerprint("ghp_replacement"), "release_manifest_digest": repeat("a", 64), "platform_setup_contract_digest": contractDigest, "workflow_cli_sha256": repeat("c", 64), "release_bundled_files_digest": repeat("d", 64)}}},
		ExpectedResults: []setupcontract.ExpectedResult{{ID: "ready", Kind: "platform_readiness", Subject: layout.Root, Expected: "ready"}},
	}
	rawB, _ := json.Marshal(planB)
	_, canonicalB, digestB, err := setupcontract.ParsePlan(rawB)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &HostAdapter{Layout: layout, DeleteCleanupBranchWithLease: func(_ context.Context, gotRepository, gotBranch, gotHead string, gotCredential onboarding.GitCredential) error {
		if gotRepository != repository || gotBranch != branch || gotHead != head || gotCredential.Token != "ghp_replacement" {
			t.Fatalf("cleanup was not bound to the approved replacement credential: repository=%q branch=%q head=%q credential=%#v", gotRepository, gotBranch, gotHead, gotCredential)
		}
		deletes++
		return nil
	}}
	result, applyErr := (&Engine{Adapter: adapter, SecretInput: &SecretInput{Reader: bytes.NewBufferString("ghp_replacement")}, GitHubCredentialVerifier: &githubcredential.Verifier{APIBase: server.URL, Client: server.Client()}, ExpectedResultVerifier: passingExpectedResultVerifier, PlatformPreconditionVerifier: passingPlatformPreconditionVerifier}).Apply(context.Background(), canonicalB, digestB)
	if applyErr != nil || result.Status != setupcontract.ExecutionSucceeded || deletes != 1 {
		t.Fatalf("replacement result=%#v err=%v deletes=%d", result, applyErr, deletes)
	}
}

func TestEngineFailsClosedUntilPlatformReadyExpectedResultIsVerified(t *testing.T) {
	home := filepath.Join(t.TempDir(), "WorkflowHome")
	plan := testPlan(home)
	raw, _ := json.Marshal(plan)
	_, _, digest, err := setupcontract.ParsePlan(raw)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &fakeAdapter{states: map[string]setupcontract.EffectStatus{"first": setupcontract.EffectSatisfied, "second": setupcontract.EffectSatisfied}}
	result, err := (&Engine{Adapter: adapter, PlatformPreconditionVerifier: passingPlatformPreconditionVerifier}).Apply(context.Background(), raw, digest)
	if err == nil || result.Status != setupcontract.ExecutionIncomplete || !strings.Contains(err.Error(), "Platform Ready verifier") {
		t.Fatalf("missing verifier result=%#v err=%v", result, err)
	}
	verified := false
	result, err = (&Engine{Adapter: adapter, PlatformPreconditionVerifier: passingPlatformPreconditionVerifier, ExpectedResultVerifier: func(_ context.Context, got setupcontract.Plan, expected setupcontract.ExpectedResult) error {
		verified = got.PlanID == plan.PlanID && expected.Kind == "platform_readiness" && expected.Subject == home
		return nil
	}}).Apply(context.Background(), raw, digest)
	if err != nil || result.Status != setupcontract.ExecutionSucceeded || !verified {
		t.Fatalf("verified result=%#v err=%v called=%t", result, err, verified)
	}
}

func TestEngineFinalGateRejectsCleanupCreatedDuringExpectedResultVerification(t *testing.T) {
	home := filepath.Join(t.TempDir(), "WorkflowHome")
	plan := testPlan(home)
	raw, _ := json.Marshal(plan)
	_, _, digest, err := setupcontract.ParsePlan(raw)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &cleanupRetryAdapter{fakeAdapter: &fakeAdapter{states: map[string]setupcontract.EffectStatus{"first": setupcontract.EffectSatisfied, "second": setupcontract.EffectSatisfied}}}
	result, err := (&Engine{Adapter: adapter, PlatformPreconditionVerifier: passingPlatformPreconditionVerifier, ExpectedResultVerifier: func(ctx context.Context, got setupcontract.Plan, expected setupcontract.ExpectedResult) error {
		layout, resolveErr := workflowhome.Resolve(got.Target.WorkflowHome)
		if resolveErr != nil {
			return resolveErr
		}
		db, openErr := store.Open(ctx, filepath.Join(layout.State, "workflow.db"))
		if openErr != nil {
			return openErr
		}
		defer db.Close()
		return db.RecordSetupCleanupObligation(ctx, store.SetupCleanupObligation{PlanID: got.PlanID, PlanDigestSHA256: digest, EffectID: expected.ID, ObligationID: expected.ID + ":docker-container", Kind: "docker_container", Resource: `{"value":"workflow-setup-docker-aaaaaaaaaaaa"}`, Status: store.CleanupPending, UpdatedAt: testTime()})
	}}).Apply(context.Background(), raw, digest)
	if err == nil || result.Status != setupcontract.ExecutionIncomplete || !strings.Contains(err.Error(), "remain pending") {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestEngineChecksSatisfiedPlatformComponentsBeforeFirstMutation(t *testing.T) {
	home := filepath.Join(t.TempDir(), "WorkflowHome")
	plan := testPlan(home)
	raw, _ := json.Marshal(plan)
	_, _, digest, err := setupcontract.ParsePlan(raw)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &fakeAdapter{states: map[string]setupcontract.EffectStatus{}}
	result, err := (&Engine{Adapter: adapter, PlatformPreconditionVerifier: func(context.Context, setupcontract.Plan) error { return errors.New("satisfied Docker drifted") }, ExpectedResultVerifier: passingExpectedResultVerifier}).Apply(context.Background(), raw, digest)
	if err == nil || result.Status != setupcontract.ExecutionDrifted || len(adapter.applied) != 0 || !strings.Contains(err.Error(), "Docker drifted") {
		t.Fatalf("result=%#v err=%v applied=%v", result, err, adapter.applied)
	}
}

func passingExpectedResultVerifier(context.Context, setupcontract.Plan, setupcontract.ExpectedResult) error {
	return nil
}

func passingPlatformPreconditionVerifier(context.Context, setupcontract.Plan) error { return nil }

func recordVerifiedOnboardingIdentity(t *testing.T, layout workflowhome.Layout, owner, login string) {
	t.Helper()
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(context.Background(), filepath.Join(layout.State, "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.RecordGitHubPATVerification(context.Background(), store.GitHubPATVerification{FingerprintSHA256: repeat("a", 64), Login: login, UserID: 1, Owner: owner, Scopes: []string{"repo", "workflow"}, CredentialPath: layout.CredentialFile, Status: "verified", VerifiedAt: testTime()}); err != nil {
		t.Fatal(err)
	}
}

func TestEngineRestoresDurableEffectEvidenceBeforeRetryReadback(t *testing.T) {
	home := filepath.Join(t.TempDir(), "WorkflowHome")
	plan := testPlan(home)
	raw, _ := json.Marshal(plan)
	_, _, digest, err := setupcontract.ParsePlan(raw)
	if err != nil {
		t.Fatal(err)
	}
	mergeHead := repeat("a", 40)
	adapter := &fakeAdapter{states: map[string]setupcontract.EffectStatus{"first": setupcontract.EffectSatisfied, "second": setupcontract.EffectSatisfied}, evidence: map[string]string{"first": onboardingMergeHeadEvidence + mergeHead}}
	engine := Engine{Adapter: adapter, ExpectedResultVerifier: passingExpectedResultVerifier, PlatformPreconditionVerifier: passingPlatformPreconditionVerifier}
	if result, err := engine.Apply(context.Background(), raw, digest); err != nil || result.Status != setupcontract.ExecutionSucceeded {
		t.Fatalf("first result=%#v err=%v", result, err)
	}
	adapter.restored = nil
	if result, err := engine.Apply(context.Background(), raw, digest); err != nil || result.Status != setupcontract.ExecutionSucceeded {
		t.Fatalf("retry result=%#v err=%v", result, err)
	}
	found := false
	for _, result := range adapter.restored {
		if result.EffectID == "first" && result.Evidence == onboardingMergeHeadEvidence+mergeHead {
			found = true
		}
	}
	if !found {
		t.Fatalf("retry did not restore merge evidence: %#v", adapter.restored)
	}
}

func TestEnginePersistsRepositoryCreateIntentBeforeUnknownExternalOutcomeAndRestoresIt(t *testing.T) {
	ctx := context.Background()
	layout, err := workflowhome.Resolve(filepath.Join(t.TempDir(), "WorkflowHome"))
	if err != nil {
		t.Fatal(err)
	}
	recordVerifiedOnboardingIdentity(t, layout, "owner", "owner")
	repositoryPath := filepath.Join(t.TempDir(), "repository")
	plan := setupcontract.Plan{
		SchemaVersion: 1, PlanID: "create-uncertain", Kind: setupcontract.RepositoryOnboarding,
		Target:        setupcontract.Target{WorkflowHome: layout.Root, RepositoryPath: repositoryPath, GitHubRepository: "owner/repo"},
		Preconditions: []setupcontract.Precondition{{ID: "head", Kind: "git_head", Subject: repositoryPath, Expected: "ok"}},
		Effects: []setupcontract.Effect{
			{ID: "create-repository", Kind: "create_repository", Subject: "owner/repo", Action: "create", Parameters: map[string]string{"owner": "owner", "authenticated_login": "owner", "name": "repo", "private": "true", "approval_absent_repository": "owner/repo"}},
			{ID: "admit", Kind: "repository_admission", Subject: "owner/repo", Action: "verify_and_record", Parameters: map[string]string{"default_branch": "main", "manifest_digest": repeat("b", 64), "contract_version": "1"}},
		},
		ExpectedResults: []setupcontract.ExpectedResult{{ID: "ready", Kind: "repository_admission", Subject: "owner/repo", Expected: repeat("b", 64)}},
	}
	raw, _ := json.Marshal(plan)
	_, _, digest, err := setupcontract.ParsePlan(raw)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &repositoryCreateAttemptAdapter{
		fakeAdapter:  &fakeAdapter{states: map[string]setupcontract.EffectStatus{"admit": setupcontract.EffectSatisfied}},
		databasePath: filepath.Join(layout.State, "workflow.db"), planID: plan.PlanID, digest: digest, repository: "owner/repo", private: true,
		applyErr: context.DeadlineExceeded,
	}
	engine := Engine{Adapter: adapter, ResolveCodexAuth: func(context.Context) (string, error) { return filepath.Join(t.TempDir(), "auth.json"), nil }}
	first, err := engine.Apply(ctx, raw, digest)
	if !errors.Is(err, context.DeadlineExceeded) || first.Status != setupcontract.ExecutionIncomplete || !adapter.startedSeen {
		t.Fatalf("unknown create result=%#v err=%v started-before-call=%t", first, err, adapter.startedSeen)
	}
	adapter.created = true
	adapter.states["create-repository"] = setupcontract.EffectRequired
	second, err := engine.Apply(ctx, raw, digest)
	if err != nil || second.Status != setupcontract.ExecutionSucceeded {
		t.Fatalf("same-plan forward repair=%#v err=%v", second, err)
	}
	if len(adapter.applied) != 0 {
		t.Fatalf("repository creation was retried after exact readback: %v", adapter.applied)
	}
}

func TestEngineResumesCreateAfterCrashImmediatelyFollowingStartedEvidence(t *testing.T) {
	ctx := context.Background()
	layout, err := workflowhome.Resolve(filepath.Join(t.TempDir(), "WorkflowHome"))
	if err != nil || layout.Ensure() != nil {
		t.Fatal(err)
	}
	repositoryPath := filepath.Join(t.TempDir(), "repository")
	plan := setupcontract.Plan{
		SchemaVersion: 1, PlanID: "create-crash-after-started", Kind: setupcontract.RepositoryOnboarding,
		Target:        setupcontract.Target{WorkflowHome: layout.Root, RepositoryPath: repositoryPath, GitHubRepository: "owner/repo"},
		Preconditions: []setupcontract.Precondition{{ID: "head", Kind: "git_head", Subject: repositoryPath, Expected: "ok"}},
		Effects: []setupcontract.Effect{
			{ID: "create-repository", Kind: "create_repository", Subject: "owner/repo", Action: "create", Parameters: map[string]string{"owner": "owner", "authenticated_login": "owner", "name": "repo", "private": "true", "approval_absent_repository": "owner/repo"}},
			{ID: "admit", Kind: "repository_admission", Subject: "owner/repo", Action: "verify_and_record", Parameters: map[string]string{"default_branch": "main", "manifest_digest": repeat("b", 64), "contract_version": "1"}},
		},
		ExpectedResults: []setupcontract.ExpectedResult{{ID: "ready", Kind: "repository_admission", Subject: "owner/repo", Expected: repeat("b", 64)}},
	}
	raw, _ := json.Marshal(plan)
	_, canonical, digest, err := setupcontract.ParsePlan(raw)
	if err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(layout.State, "workflow.db")
	database, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	now := testTime()
	if err := database.RecordSetupPlan(ctx, store.SetupPlanRecord{PlanID: plan.PlanID, Kind: string(plan.Kind), SchemaVersion: plan.SchemaVersion, Target: plan.Target.RepositoryPath, DigestSHA256: digest, CanonicalJSON: string(canonical), Projection: Project(plan, digest), CreatedAt: now}); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.AppendSetupRepositoryCreateAttemptEvent(ctx, store.SetupRepositoryCreateAttemptEvent{PlanID: plan.PlanID, PlanDigestSHA256: digest, EffectID: "create-repository", ExecutionAttempt: 1, Event: store.RepositoryCreateStarted, Owner: "owner", Name: "repo", Private: true, ApprovalAbsentRepository: "owner/repo", RecordedAt: now}); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.RecordGitHubPATVerification(ctx, store.GitHubPATVerification{FingerprintSHA256: repeat("a", 64), Login: "owner", UserID: 1, Owner: "owner", Scopes: []string{"repo", "workflow"}, CredentialPath: layout.CredentialFile, Status: "verified", VerifiedAt: now}); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	adapter := &repositoryCreateAttemptAdapter{fakeAdapter: &fakeAdapter{states: map[string]setupcontract.EffectStatus{"admit": setupcontract.EffectSatisfied}}, databasePath: databasePath, planID: plan.PlanID, digest: digest, repository: "owner/repo", private: true}
	result, err := (&Engine{Adapter: adapter, Now: func() time.Time { return now.Add(time.Minute) }, ResolveCodexAuth: func(context.Context) (string, error) { return filepath.Join(t.TempDir(), "auth.json"), nil }}).Apply(ctx, raw, digest)
	if err != nil || result.Status != setupcontract.ExecutionSucceeded || adapter.applyCalls != 1 {
		t.Fatalf("crash-after-started resume result=%#v err=%v applyCalls=%d", result, err, adapter.applyCalls)
	}
}

func TestEngineRejectsDigestBeforeMutation(t *testing.T) {
	home := filepath.Join(t.TempDir(), "WorkflowHome")
	plan := testPlan(home)
	raw, _ := json.Marshal(plan)
	adapter := &fakeAdapter{states: map[string]setupcontract.EffectStatus{}}
	_, err := (&Engine{Adapter: adapter}).Apply(context.Background(), raw, repeat("0", 64))
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("err=%v", err)
	}
	if len(adapter.applied) != 0 {
		t.Fatal("digest mismatch mutated host")
	}
}

func TestEngineRejectsOnboardingIdentityDriftBeforeGitHubMutation(t *testing.T) {
	for _, test := range []struct {
		name               string
		persistedOwner     string
		persistedLogin     string
		authenticatedLogin string
	}{
		{name: "target owner differs", persistedOwner: "attacker", persistedLogin: "alice", authenticatedLogin: "alice"},
		{name: "repository creator login differs", persistedOwner: "owner", persistedLogin: "alice", authenticatedLogin: "mallory"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			layout, err := workflowhome.Resolve(filepath.Join(t.TempDir(), "WorkflowHome"))
			if err != nil || layout.Ensure() != nil {
				t.Fatal(err)
			}
			database, err := store.Open(ctx, filepath.Join(layout.State, "workflow.db"))
			if err != nil {
				t.Fatal(err)
			}
			if err := database.RecordGitHubPATVerification(ctx, store.GitHubPATVerification{FingerprintSHA256: repeat("a", 64), Login: test.persistedLogin, UserID: 1, Owner: test.persistedOwner, Scopes: []string{"repo", "workflow"}, CredentialPath: layout.CredentialFile, Status: "verified", VerifiedAt: testTime()}); err != nil {
				database.Close()
				t.Fatal(err)
			}
			if err := database.Close(); err != nil {
				t.Fatal(err)
			}
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
			defer server.Close()
			plan := setupcontract.Plan{
				SchemaVersion: 1, PlanID: "identity-drift", Kind: setupcontract.RepositoryOnboarding,
				Target:        setupcontract.Target{WorkflowHome: layout.Root, RepositoryPath: `C:\repo`, GitHubRepository: "owner/repo"},
				Preconditions: []setupcontract.Precondition{{ID: "head", Kind: "git_head", Subject: `C:\repo`, Expected: "ok"}},
				Effects: []setupcontract.Effect{
					{ID: "create", Kind: "create_repository", Subject: "owner/repo", Action: "create", Parameters: map[string]string{"owner": "owner", "authenticated_login": test.authenticatedLogin, "name": "repo", "private": "true", "approval_absent_repository": "owner/repo"}},
					{ID: "admit", Kind: "repository_admission", Subject: "owner/repo", Action: "verify_and_record", Parameters: map[string]string{"default_branch": "main", "manifest_digest": repeat("b", 64), "contract_version": "1"}},
				},
				ExpectedResults: []setupcontract.ExpectedResult{{ID: "ready", Kind: "repository_admission", Subject: "owner/repo", Expected: repeat("b", 64)}},
			}
			raw, _ := json.Marshal(plan)
			_, _, digest, err := setupcontract.ParsePlan(raw)
			if err != nil {
				t.Fatal(err)
			}
			adapter := HostAdapter{Layout: layout, GitHub: workflowgithub.NewClient(server.URL, "token", server.Client()).WithRepositoryOwner("owner"), CreatedRepositories: map[string]bool{}}
			result, applyErr := (&Engine{Adapter: adapter}).Apply(ctx, raw, digest)
			if applyErr == nil || result.Status != "" || requests != 0 {
				t.Fatalf("identity drift reached mutation: result=%#v err=%v requests=%d", result, applyErr, requests)
			}
		})
	}
}

func TestEngineRecordsAndRepairsRepositoryRuntimeConfiguration(t *testing.T) {
	ctx := context.Background()
	layout, err := workflowhome.Resolve(filepath.Join(t.TempDir(), "WorkflowHome"))
	if err != nil {
		t.Fatal(err)
	}
	recordVerifiedOnboardingIdentity(t, layout, "owner", "owner")
	repositoryPath := filepath.Join(t.TempDir(), "repository")
	plan := setupcontract.Plan{
		SchemaVersion: 1, PlanID: "onboarding", Kind: setupcontract.RepositoryOnboarding,
		Target:        setupcontract.Target{WorkflowHome: layout.Root, RepositoryPath: repositoryPath, GitHubRepository: "owner/repo"},
		Preconditions: []setupcontract.Precondition{{ID: "head", Kind: "git_head", Subject: repositoryPath, Expected: repeat("a", 40)}},
		Effects: []setupcontract.Effect{{ID: "admit", Kind: "repository_admission", Subject: "owner/repo", Action: "verify_and_record", Parameters: map[string]string{
			"default_branch": "main", "manifest_digest": repeat("b", 64), "contract_version": "1",
		}}},
		ExpectedResults: []setupcontract.ExpectedResult{{ID: "ready", Kind: "repository_admission", Subject: "owner/repo", Expected: repeat("b", 64)}},
	}
	raw, _ := json.Marshal(plan)
	_, _, digest, err := setupcontract.ParsePlan(raw)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &fakeAdapter{states: map[string]setupcontract.EffectStatus{}}
	engine := Engine{Adapter: adapter, ResolveCodexAuth: func(context.Context) (string, error) { return filepath.Join(t.TempDir(), "auth.json"), nil }}
	result, err := engine.Apply(ctx, raw, digest)
	if err != nil || result.Status != setupcontract.ExecutionSucceeded {
		t.Fatalf("apply = %#v, %v", result, err)
	}
	databasePath := filepath.Join(layout.State, "workflow.db")
	database, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	config, err := database.RepositoryRuntimeConfiguration(ctx, "owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	if config.SourcePath != repositoryPath || config.DefaultBranch != "main" || config.RootIssueNumber != 0 || config.UpdatedAt.IsZero() {
		t.Fatalf("runtime configuration = %#v", config)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	rawDatabase, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rawDatabase.ExecContext(ctx, "DELETE FROM repository_runtime_configurations WHERE repository='owner/repo'"); err != nil {
		rawDatabase.Close()
		t.Fatal(err)
	}
	if err := rawDatabase.Close(); err != nil {
		t.Fatal(err)
	}
	// A previously successful plan must repair a missing derived runtime record
	// instead of short-circuiting on Repository Admission alone.
	result, err = engine.Apply(ctx, raw, digest)
	if err != nil || result.Status != setupcontract.ExecutionSucceeded {
		t.Fatalf("repair = %#v, %v", result, err)
	}
	database, err = store.Open(ctx, filepath.Join(layout.State, "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	repaired, err := database.RepositoryRuntimeConfiguration(ctx, "owner/repo")
	if err != nil || repaired.SourcePath != repositoryPath || repaired.UpdatedAt.Before(config.UpdatedAt) {
		t.Fatalf("repaired runtime configuration = %#v, %v", repaired, err)
	}
	if len(adapter.read) < 2 {
		t.Fatalf("successful retry did not read back every authorized effect: %v", adapter.read)
	}
}

func TestEngineReadmissionPreservesExistingRepositoryRuntimeSettings(t *testing.T) {
	ctx := context.Background()
	layout, err := workflowhome.Resolve(filepath.Join(t.TempDir(), "WorkflowHome"))
	if err != nil {
		t.Fatal(err)
	}
	recordVerifiedOnboardingIdentity(t, layout, "owner", "owner")
	database, err := store.Open(ctx, filepath.Join(layout.State, "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	previous := store.RepositoryRuntimeConfiguration{
		Repository: "owner/repo", DefaultBranch: "master", SourcePath: filepath.Join(t.TempDir(), "old-source"),
		RootIssueNumber: 42, WorkspaceRoot: filepath.Join(t.TempDir(), "custom-workspaces"), StateRoot: filepath.Join(t.TempDir(), "custom-state"),
		CodexAuthFile: filepath.Join(t.TempDir(), "custom-auth.json"), GitHubAPIURL: "https://github.example.test/api/v3",
		PollInterval: 17 * time.Second, WorkspaceRetention: 19 * 24 * time.Hour, MaxParallelRuns: 7, UpdatedAt: testTime(),
	}
	if err := database.RecordRepositoryAdmission(ctx, store.RepositoryAdmission{Repository: "owner/repo", OnboardingPlanDigestSHA256: repeat("b", 64), ContractVersion: "1", ManifestDigestSHA256: repeat("b", 64), Eligible: true, VerifiedAt: testTime()}); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.RecordRepositoryRuntimeConfiguration(ctx, previous); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	repositoryPath := filepath.Join(t.TempDir(), "new-source")
	plan := setupcontract.Plan{
		SchemaVersion: 1, PlanID: "readmit-runtime", Kind: setupcontract.RepositoryOnboarding,
		Target:        setupcontract.Target{WorkflowHome: layout.Root, RepositoryPath: repositoryPath, GitHubRepository: "owner/repo"},
		Preconditions: []setupcontract.Precondition{{ID: "head", Kind: "git_head", Subject: repositoryPath, Expected: repeat("a", 40)}},
		Effects: []setupcontract.Effect{{ID: "admit", Kind: "repository_admission", Subject: "owner/repo", Action: "verify_and_record", Parameters: map[string]string{
			"default_branch": "main", "manifest_digest": repeat("c", 64), "contract_version": "1",
		}}},
		ExpectedResults: []setupcontract.ExpectedResult{{ID: "ready", Kind: "repository_admission", Subject: "owner/repo", Expected: repeat("c", 64)}},
	}
	raw, _ := json.Marshal(plan)
	_, _, digest, err := setupcontract.ParsePlan(raw)
	if err != nil {
		t.Fatal(err)
	}
	engine := Engine{Adapter: &fakeAdapter{states: map[string]setupcontract.EffectStatus{}}, ResolveCodexAuth: func(context.Context) (string, error) {
		return filepath.Join(t.TempDir(), "replacement-auth.json"), nil
	}}
	if result, err := engine.Apply(ctx, raw, digest); err != nil || result.Status != setupcontract.ExecutionSucceeded {
		t.Fatalf("readmission = %#v, %v", result, err)
	}
	database, err = store.Open(ctx, filepath.Join(layout.State, "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	got, err := database.RepositoryRuntimeConfiguration(ctx, "owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	if got.Repository != "owner/repo" || got.DefaultBranch != "main" || got.SourcePath != repositoryPath {
		t.Fatalf("approved runtime identity was not refreshed: %#v", got)
	}
	if got.RootIssueNumber != previous.RootIssueNumber || got.WorkspaceRoot != previous.WorkspaceRoot || got.StateRoot != previous.StateRoot || got.CodexAuthFile != previous.CodexAuthFile || got.GitHubAPIURL != previous.GitHubAPIURL || got.PollInterval != previous.PollInterval || got.WorkspaceRetention != previous.WorkspaceRetention || got.MaxParallelRuns != previous.MaxParallelRuns {
		t.Fatalf("readmission replaced existing operational settings:\nwant=%#v\n got=%#v", previous, got)
	}
	admission, err := database.RepositoryAdmission(ctx, "owner/repo")
	if err != nil || admission.ManifestDigestSHA256 != repeat("c", 64) || admission.OnboardingPlanDigestSHA256 != digest || !admission.Eligible {
		t.Fatalf("approved admission identity was not refreshed: %#v, %v", admission, err)
	}
}

func TestEngineChecksPlatformReleasePreconditionBeforeMutation(t *testing.T) {
	home := filepath.Join(t.TempDir(), "WorkflowHome")
	plan := testPlan(home)
	plan.Preconditions = []setupcontract.Precondition{{ID: "release", Kind: "platform_release", Subject: "platform-v1.0.0", Expected: repeat("a", 64)}}
	plan.Effects = append(plan.Effects, setupcontract.Effect{ID: "record", Kind: "platform_installation", Subject: home, Action: "record", Parameters: map[string]string{"version": "1.0.0", "release_manifest_digest": repeat("b", 64), "platform_setup_contract_json": `{}`, "platform_setup_contract_digest": repeat("c", 64), "workflow_cli_sha256": repeat("d", 64), "release_bundled_files_json": `[]`, "release_bundled_files_digest": repeat("e", 64)}})
	raw, _ := json.Marshal(plan)
	_, _, digest, err := setupcontract.ParsePlan(raw)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &fakeAdapter{states: map[string]setupcontract.EffectStatus{}}
	result, err := (&Engine{Adapter: adapter}).Apply(context.Background(), raw, digest)
	if err == nil || result.Status != setupcontract.ExecutionDrifted || len(adapter.applied) != 0 {
		t.Fatalf("unchecked Platform Release precondition: result=%#v err=%v applied=%v", result, err, adapter.applied)
	}
}

func TestEngineRejectsCrossUserPlatformPlanBeforeCreatingWorkflowHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "WorkflowHome")
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	expected, _ := json.Marshal(hostIdentityPrecondition{UserID: "S-1-5-21-other-user", Username: current.Username, WorkflowHome: home, WorkflowHomeOwnerID: "S-1-5-21-other-user"})
	plan := testPlan(home)
	plan.Preconditions = []setupcontract.Precondition{{ID: "host", Kind: "host_identity", Subject: "current-user", Expected: string(expected)}}
	raw, _ := json.Marshal(plan)
	_, _, digest, err := setupcontract.ParsePlan(raw)
	if err != nil {
		t.Fatal(err)
	}
	result, applyErr := (&Engine{Adapter: HostAdapter{Layout: workflowhome.Layout{Root: home}}}).Apply(context.Background(), raw, digest)
	if applyErr == nil || result.Status != "" {
		t.Fatalf("cross-user preflight result=%#v err=%v", result, applyErr)
	}
	if _, err := os.Stat(home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cross-user apply created Workflow Home before identity verification: %v", err)
	}
}

func TestEngineAppliesOnlyDigestBoundPlatformInstallationTransition(t *testing.T) {
	for _, transition := range []struct {
		name          string
		targetVersion string
	}{
		{name: "version upgrade", targetVersion: "2.0.0"},
		{name: "same-version pin repair", targetVersion: "1.0.0"},
	} {
		t.Run(transition.name, func(t *testing.T) {
			for _, test := range []struct {
				name       string
				authorized bool
				wantStatus setupcontract.ExecutionStatus
			}{
				{name: "unapproved changed pins conflict", wantStatus: setupcontract.ExecutionDrifted},
				{name: "approved old pins transition", authorized: true, wantStatus: setupcontract.ExecutionIncomplete},
			} {
				t.Run(test.name, func(t *testing.T) {
					ctx := context.Background()
					layout, err := workflowhome.Resolve(filepath.Join(t.TempDir(), "WorkflowHome"))
					if err != nil || layout.Ensure() != nil {
						t.Fatal(err)
					}
					executable := filepath.Join(t.TempDir(), "workflow.exe")
					if err := os.WriteFile(executable, []byte("new workflow"), 0o700); err != nil {
						t.Fatal(err)
					}
					cliSum := sha256.Sum256([]byte("new workflow"))
					cliDigest := hex.EncodeToString(cliSum[:])
					if err := (workflowhome.Installation{Layout: layout}).InstallVersion(transition.targetVersion, executable, cliDigest); err != nil {
						t.Fatal(err)
					}
					bundleRaw, _ := json.Marshal([]platformrelease.BundledFile{{Path: "bin/workflow.exe", SHA256: cliDigest}})
					bundleCanonical, bundleDigest, _ := setupcontract.Canonicalize(bundleRaw)
					old := store.PlatformInstallation{PlatformVersion: "1.0.0", ReleaseManifestDigestSHA256: repeat("1", 64), PlatformSetupContractDigestSHA256: repeat("2", 64), WorkflowCLISHA256: repeat("3", 64), ReleaseBundledFilesJSON: string(bundleCanonical), ReleaseBundledFilesDigestSHA256: bundleDigest, ControlPlanePlanDigestSHA256: repeat("4", 64), WorkflowHome: layout.Root}
					old.InstalledAt, old.VerifiedAt = testTime(), testTime()
					database, err := store.Open(ctx, filepath.Join(layout.State, "workflow.db"))
					if err != nil || database.RecordPlatformInstallation(ctx, old) != nil || database.AuthorizeControlPlane(ctx, old, old.ControlPlanePlanDigestSHA256) != nil {
						t.Fatalf("record old installation: %v", err)
					}
					database.Close()
					contractRaw := validPlatformSetupContractJSON(t)
					contractCanonical, contractDigest, _ := setupcontract.Canonicalize(contractRaw)
					plan := setupcontract.Plan{SchemaVersion: 1, PlanID: "transition-" + strings.ReplaceAll(transition.name, " ", "-"), Kind: setupcontract.PlatformBootstrap, Target: setupcontract.Target{WorkflowHome: layout.Root}, Preconditions: []setupcontract.Precondition{
						{ID: "release", Kind: "platform_release", Subject: "platform-v2.0.0", Expected: repeat("5", 64)},
						{ID: "contract", Kind: "platform_setup_contract", Subject: "platform-v2.0.0", Expected: contractDigest},
					}, Effects: []setupcontract.Effect{
						{ID: "record", Kind: "platform_installation", Subject: layout.Root, Action: "record", Parameters: map[string]string{"version": transition.targetVersion, "release_manifest_digest": repeat("5", 64), "platform_setup_contract_json": string(contractCanonical), "platform_setup_contract_digest": contractDigest, "workflow_cli_sha256": cliDigest, "release_bundled_files_json": string(bundleCanonical), "release_bundled_files_digest": bundleDigest}},
						{ID: "control-plane", Kind: "control_plane", Subject: layout.Root, Action: "start", Parameters: map[string]string{"version": transition.targetVersion, "release_manifest_digest": repeat("5", 64), "platform_setup_contract_digest": contractDigest, "workflow_cli_sha256": cliDigest, "release_bundled_files_digest": bundleDigest}},
					}, ExpectedResults: []setupcontract.ExpectedResult{{ID: "ready", Kind: "platform_readiness", Subject: layout.Root, Expected: "ready"}}}
					if test.authorized {
						priorRaw, _ := json.Marshal(map[string]string{"version": old.PlatformVersion, "release_manifest_digest": old.ReleaseManifestDigestSHA256, "platform_setup_contract_digest": old.PlatformSetupContractDigestSHA256, "workflow_cli_sha256": old.WorkflowCLISHA256, "release_bundled_files_digest": old.ReleaseBundledFilesDigestSHA256, "control_plane_plan_digest_sha256": old.ControlPlanePlanDigestSHA256})
						_, priorDigest, _ := setupcontract.Canonicalize(priorRaw)
						plan.Preconditions = append(plan.Preconditions, setupcontract.Precondition{ID: "installed", Kind: "platform_installation", Subject: layout.Root, Expected: priorDigest})
					}
					raw, _ := json.Marshal(plan)
					_, _, digest, err := setupcontract.ParsePlan(raw)
					if err != nil {
						t.Fatal(err)
					}
					adapter := &fakeAdapter{states: map[string]setupcontract.EffectStatus{}, fail: "control-plane"}
					engine := &Engine{Adapter: adapter, ExpectedResultVerifier: passingExpectedResultVerifier, PlatformPreconditionVerifier: passingPlatformPreconditionVerifier}
					result, applyErr := engine.Apply(ctx, raw, digest)
					if result.Status != test.wantStatus || applyErr == nil {
						t.Fatalf("result=%#v err=%v", result, applyErr)
					}
					if test.authorized {
						database, err = store.Open(ctx, filepath.Join(layout.State, "workflow.db"))
						if err != nil {
							t.Fatal(err)
						}
						got, err := database.PlatformInstallation(ctx)
						if err != nil || got.PlatformVersion != transition.targetVersion || got.ReleaseManifestDigestSHA256 != repeat("5", 64) || got.ControlPlanePlanDigestSHA256 != digest {
							t.Fatalf("upgraded installation=%#v err=%v", got, err)
						}
						if err := database.Close(); err != nil {
							t.Fatal(err)
						}
						withoutEvidence := plan
						withoutEvidence.PlanID += "-without-evidence"
						withoutEvidenceRaw, _ := json.Marshal(withoutEvidence)
						_, _, withoutEvidenceDigest, parseErr := setupcontract.ParsePlan(withoutEvidenceRaw)
						if parseErr != nil {
							t.Fatal(parseErr)
						}
						withoutEvidenceResult, withoutEvidenceErr := (&Engine{Adapter: &fakeAdapter{states: map[string]setupcontract.EffectStatus{}}, ExpectedResultVerifier: passingExpectedResultVerifier, PlatformPreconditionVerifier: passingPlatformPreconditionVerifier}).Apply(ctx, withoutEvidenceRaw, withoutEvidenceDigest)
						if withoutEvidenceErr == nil || withoutEvidenceResult.Status != setupcontract.ExecutionDrifted {
							t.Fatalf("exact-new installation without same-plan evidence result=%#v err=%v", withoutEvidenceResult, withoutEvidenceErr)
						}
						// A failed later effect may leave the installation effect durably
						// recorded. The same approved plan must accept its exact new pins
						// on retry even though its transition precondition names the old pins.
						adapter.fail = ""
						result, applyErr = engine.Apply(ctx, raw, digest)
						if applyErr != nil || result.Status != setupcontract.ExecutionSucceeded {
							t.Fatalf("Control Plane retry from exact-new installation result=%#v err=%v", result, applyErr)
						}

						database, err = store.Open(ctx, filepath.Join(layout.State, "workflow.db"))
						if err != nil {
							t.Fatal(err)
						}
						third := got
						third.PlatformVersion = "3.0.0"
						third.ReleaseManifestDigestSHA256 = repeat("9", 64)
						if err := database.RecordPlatformInstallation(ctx, third); err != nil {
							database.Close()
							t.Fatal(err)
						}
						if err := database.Close(); err != nil {
							t.Fatal(err)
						}
						result, applyErr = (&Engine{Adapter: &fakeAdapter{states: map[string]setupcontract.EffectStatus{}}, ExpectedResultVerifier: passingExpectedResultVerifier, PlatformPreconditionVerifier: passingPlatformPreconditionVerifier}).Apply(ctx, raw, digest)
						if applyErr == nil || result.Status != setupcontract.ExecutionDrifted {
							t.Fatalf("third-state retry result=%#v err=%v", result, applyErr)
						}
					}
				})
			}
		})
	}
}

func validPlatformSetupContractJSON(t *testing.T) []byte {
	t.Helper()
	contract := platformrelease.PlatformSetupContract{WorkflowHomeDefault: `%LOCALAPPDATA%\AgentWorkflow`, Credential: platformrelease.CredentialContract{Kind: "classic-pat", RequiredScopes: []string{"repo", "workflow"}, OwnerBinding: "single-owner", PlaintextRelativePath: `state\credentials\github.pat`}, Docker: platformrelease.DockerDependency{Version: "4.45.0", InstallerURL: "https://example.test/docker.exe", WindowsAMD64SHA256: repeat("6", 64)}, Worker: platformrelease.WorkerPin{Image: "ghcr.io/owner/worker@sha256:" + repeat("7", 64)}, SkillBundle: platformrelease.SkillBundleContract{Version: "2.0.0", InstallScope: "user", ManagedSkills: []string{"implement"}}, RepositoryContract: platformrelease.RepositoryContractPin{Version: "1", ManifestPath: ".workflow/repository.json", CheckName: "workflow-contract", Labels: []platformrelease.RepositoryLabel{{Name: "workflow:plan", Color: "0e8a16", Description: "plan"}}}}
	raw, err := json.Marshal(contract)
	if err != nil || contract.Validate() != nil {
		t.Fatalf("valid test contract: %v", err)
	}
	return raw
}

func testTime() time.Time { return time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC) }

func TestIncompleteOnboardingNeverLeavesEligibleAdmission(t *testing.T) {
	ctx := context.Background()
	layout, err := workflowhome.Resolve(filepath.Join(t.TempDir(), "WorkflowHome"))
	if err != nil {
		t.Fatal(err)
	}
	recordVerifiedOnboardingIdentity(t, layout, "owner", "owner")
	plan := setupcontract.Plan{SchemaVersion: 1, PlanID: "incomplete-onboarding", Kind: setupcontract.RepositoryOnboarding, Target: setupcontract.Target{WorkflowHome: layout.Root, RepositoryPath: `C:\repo`, GitHubRepository: "owner/repo"}, Preconditions: []setupcontract.Precondition{{ID: "head", Kind: "git_head", Subject: `C:\repo`, Expected: "ok"}}, Effects: []setupcontract.Effect{
		{ID: "admit", Kind: "repository_admission", Subject: "owner/repo", Action: "verify_and_record", Parameters: map[string]string{"default_branch": "main", "manifest_digest": repeat("b", 64), "contract_version": "1"}},
		{ID: "later", Kind: "github_label", Subject: "owner/repo#later", Action: "reconcile", Parameters: map[string]string{"name": "later", "color": "ffffff", "description": "later"}},
	}, ExpectedResults: []setupcontract.ExpectedResult{{ID: "ready", Kind: "repository_admission", Subject: "owner/repo", Expected: repeat("b", 64)}}}
	raw, _ := json.Marshal(plan)
	_, _, digest, err := setupcontract.ParsePlan(raw)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &fakeAdapter{states: map[string]setupcontract.EffectStatus{}, fail: "later"}
	result, err := (&Engine{Adapter: adapter, ResolveCodexAuth: func(context.Context) (string, error) { return filepath.Join(t.TempDir(), "auth.json"), nil }}).Apply(ctx, raw, digest)
	if err == nil || result.Status != setupcontract.ExecutionIncomplete {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	db, err := store.Open(ctx, filepath.Join(layout.State, "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	admission, err := db.RepositoryAdmission(ctx, "owner/repo")
	if err != nil || admission.Eligible {
		t.Fatalf("incomplete admission=%#v err=%v", admission, err)
	}
}

func testPlan(home string) setupcontract.Plan {
	pins := map[string]string{"version": "1.0.0", "sha256": repeat("d", 64), "release_manifest_digest": repeat("b", 64), "platform_setup_contract_digest": repeat("c", 64), "workflow_cli_sha256": repeat("d", 64), "release_bundled_files_digest": repeat("e", 64)}
	copyPins := func() map[string]string {
		value := map[string]string{}
		for key, item := range pins {
			value[key] = item
		}
		return value
	}
	return setupcontract.Plan{SchemaVersion: 1, PlanID: "plan-test", Kind: setupcontract.PlatformBootstrap, Target: setupcontract.Target{WorkflowHome: home}, Preconditions: []setupcontract.Precondition{{ID: "host", Kind: "host_identity", Subject: "current-user", Expected: "fake-user"}}, Effects: []setupcontract.Effect{{ID: "first", Kind: "platform_cli", Subject: "one", Action: "install", Parameters: copyPins()}, {ID: "second", Kind: "platform_cli", Subject: "two", Action: "install", Parameters: copyPins()}}, ExpectedResults: []setupcontract.ExpectedResult{{ID: "ready", Kind: "platform_readiness", Subject: home, Expected: "ready"}}}
}
func repeat(value string, count int) string {
	var b bytes.Buffer
	for i := 0; i < count; i++ {
		b.WriteString(value)
	}
	return b.String()
}
