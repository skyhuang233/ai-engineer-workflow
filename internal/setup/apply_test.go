package setup

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

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
	engine := Engine{Adapter: adapter, SecretInput: &SecretInput{Reader: bytes.NewBufferString("secret\n")}}
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
	engine := Engine{Adapter: adapter}
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
	plan.Effects = append(plan.Effects, setupcontract.Effect{ID: "record", Kind: "platform_installation", Subject: home, Action: "record", Parameters: map[string]string{"version": "1.0.0", "release_manifest_digest": repeat("b", 64), "platform_setup_contract_json": `{}`, "platform_setup_contract_digest": repeat("c", 64), "workflow_cli_sha256": repeat("d", 64)}})
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
	pins := map[string]string{"version": "1.0.0", "sha256": repeat("d", 64), "release_manifest_digest": repeat("b", 64), "platform_setup_contract_digest": repeat("c", 64), "workflow_cli_sha256": repeat("d", 64)}
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
