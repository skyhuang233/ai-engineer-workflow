package setup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestEngineRecordsAndRepairsRepositoryRuntimeConfiguration(t *testing.T) {
	ctx := context.Background()
	layout, err := workflowhome.Resolve(filepath.Join(t.TempDir(), "WorkflowHome"))
	if err != nil {
		t.Fatal(err)
	}
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

func TestEngineAppliesOnlyDigestBoundPlatformInstallationUpgrade(t *testing.T) {
	for _, test := range []struct {
		name       string
		authorized bool
		wantStatus setupcontract.ExecutionStatus
	}{
		{name: "unapproved changed pins conflict", wantStatus: setupcontract.ExecutionDrifted},
		{name: "approved old pins transition", authorized: true, wantStatus: setupcontract.ExecutionSucceeded},
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
			if err := (workflowhome.Installation{Layout: layout}).InstallVersion("2.0.0", executable, cliDigest); err != nil {
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
			plan := setupcontract.Plan{SchemaVersion: 1, PlanID: "upgrade", Kind: setupcontract.PlatformBootstrap, Target: setupcontract.Target{WorkflowHome: layout.Root}, Preconditions: []setupcontract.Precondition{
				{ID: "release", Kind: "platform_release", Subject: "platform-v2.0.0", Expected: repeat("5", 64)},
				{ID: "contract", Kind: "platform_setup_contract", Subject: "platform-v2.0.0", Expected: contractDigest},
			}, Effects: []setupcontract.Effect{{ID: "record", Kind: "platform_installation", Subject: layout.Root, Action: "record", Parameters: map[string]string{"version": "2.0.0", "release_manifest_digest": repeat("5", 64), "platform_setup_contract_json": string(contractCanonical), "platform_setup_contract_digest": contractDigest, "workflow_cli_sha256": cliDigest, "release_bundled_files_json": string(bundleCanonical), "release_bundled_files_digest": bundleDigest}}}, ExpectedResults: []setupcontract.ExpectedResult{{ID: "ready", Kind: "platform_readiness", Subject: layout.Root, Expected: "ready"}}}
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
			result, applyErr := (&Engine{Adapter: &fakeAdapter{states: map[string]setupcontract.EffectStatus{}}, ExpectedResultVerifier: passingExpectedResultVerifier, PlatformPreconditionVerifier: passingPlatformPreconditionVerifier}).Apply(ctx, raw, digest)
			if result.Status != test.wantStatus || test.authorized && applyErr != nil || !test.authorized && applyErr == nil {
				t.Fatalf("result=%#v err=%v", result, applyErr)
			}
			if test.authorized {
				database, err = store.Open(ctx, filepath.Join(layout.State, "workflow.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer database.Close()
				got, err := database.PlatformInstallation(ctx)
				if err != nil || got.PlatformVersion != "2.0.0" || got.ReleaseManifestDigestSHA256 != repeat("5", 64) {
					t.Fatalf("upgraded installation=%#v err=%v", got, err)
				}
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
