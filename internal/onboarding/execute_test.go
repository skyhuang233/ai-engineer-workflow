package onboarding

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skyhuang233/workflow/internal/setupcontract"
	"github.com/skyhuang233/workflow/internal/store"
)

type createAfterSuccessAdapter struct {
	created bool
	creates int
	fail    bool
}

func (a *createAfterSuccessAdapter) Readback(_ context.Context, effect setupcontract.Effect) (setupcontract.EffectStatus, string, error) {
	if effect.Kind == "create_repository" && !a.created {
		return setupcontract.EffectRequired, "absent", nil
	}
	return setupcontract.EffectSatisfied, "verified", nil
}
func (a *createAfterSuccessAdapter) Apply(_ context.Context, effect setupcontract.Effect) error {
	if effect.Kind == "create_repository" {
		a.creates++
		a.created = true
		if a.fail {
			a.fail = false
			return errors.New("injected after create")
		}
	}
	return nil
}

func TestExecutorRetriesCreatedRepositoryWithoutDuplicateMutation(t *testing.T) {
	home, repo := t.TempDir(), t.TempDir()
	db, err := store.Open(context.Background(), filepath.Join(home, "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	plan := setupcontract.Plan{SchemaVersion: 1, PlanID: "onboard-retry", Kind: setupcontract.RepositoryOnboarding, Target: setupcontract.Target{WorkflowHome: home, RepositoryPath: repo, GitHubRepository: "owner/repo"}, Preconditions: []setupcontract.Precondition{{ID: "release", Kind: "platform_release", Subject: home, Expected: strings.Repeat("a", 64)}}, Effects: []setupcontract.Effect{{ID: "create", Kind: "create_repository", Subject: "owner/repo", Action: "create", Parameters: map[string]string{"owner": "owner", "authenticated_login": "owner", "name": "repo", "private": "true", "approval_absent_repository": "owner/repo"}}, {ID: "admission", Kind: "repository_admission", Subject: "owner/repo", Action: "verify_and_record", Parameters: map[string]string{"default_branch": "main", "manifest_digest": strings.Repeat("b", 64), "contract_version": "1"}}}, ExpectedResults: []setupcontract.ExpectedResult{{ID: "ready", Kind: "repository_admission", Subject: "owner/repo", Expected: strings.Repeat("b", 64)}}}
	raw, _ := json.Marshal(plan)
	_, _, digest, err := setupcontract.ParsePlan(raw)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &createAfterSuccessAdapter{fail: true}
	ex := Executor{Store: db, Adapter: adapter}
	if _, err = ex.Apply(context.Background(), raw, digest, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err = ex.Apply(context.Background(), raw, digest, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if adapter.creates != 1 {
		t.Fatalf("creates=%d", adapter.creates)
	}
	values, err := db.SetupExecutionResults(context.Background(), "onboard-retry")
	if err != nil || len(values) != 2 || values[0].Attempt != 1 || values[1].Attempt != 2 {
		t.Fatalf("attempts=%#v,%v", values, err)
	}
}

type baselineAfterSuccessAdapter struct {
	pushed       bool
	pushes       int
	fail         bool
	restoredHead string
}

func (a *baselineAfterSuccessAdapter) Readback(_ context.Context, effect setupcontract.Effect) (setupcontract.EffectStatus, string, error) {
	if effect.Kind == "initial_baseline" && !a.pushed {
		return setupcontract.EffectRequired, "missing", nil
	}
	if effect.Kind == "initial_baseline" {
		a.restoredHead = strings.Repeat("c", 40)
	}
	return setupcontract.EffectSatisfied, "verified", nil
}
func (a *baselineAfterSuccessAdapter) Apply(_ context.Context, effect setupcontract.Effect) error {
	if effect.Kind == "initial_baseline" {
		a.pushes++
		a.pushed = true
		if a.fail {
			a.fail = false
			return errors.New("injected after baseline push")
		}
	}
	return nil
}
func TestExecutorRetriesPushedBaselineWithoutDuplicatePush(t *testing.T) {
	home, repo := t.TempDir(), t.TempDir()
	db, err := store.Open(context.Background(), filepath.Join(home, "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	plan := setupcontract.Plan{SchemaVersion: 1, PlanID: "onboard-baseline-retry", Kind: setupcontract.RepositoryOnboarding, Target: setupcontract.Target{WorkflowHome: home, RepositoryPath: repo, GitHubRepository: "owner/repo"}, Preconditions: []setupcontract.Precondition{{ID: "release", Kind: "platform_release", Subject: home, Expected: strings.Repeat("a", 64)}}, Effects: []setupcontract.Effect{{ID: "baseline", Kind: "initial_baseline", Subject: repo, Action: "commit_and_push", Parameters: map[string]string{"branch": "main", "files_json": "[]", "repository": "owner/repo", "source_url": "https://github.com/owner/repo.git"}}, {ID: "admission", Kind: "repository_admission", Subject: "owner/repo", Action: "verify_and_record", Parameters: map[string]string{"default_branch": "main", "manifest_digest": strings.Repeat("b", 64), "contract_version": "1"}}}, ExpectedResults: []setupcontract.ExpectedResult{{ID: "ready", Kind: "repository_admission", Subject: "owner/repo", Expected: strings.Repeat("b", 64)}}}
	raw, _ := json.Marshal(plan)
	_, _, digest, err := setupcontract.ParsePlan(raw)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &baselineAfterSuccessAdapter{fail: true}
	ex := Executor{Store: db, Adapter: adapter}
	if _, err = ex.Apply(context.Background(), raw, digest, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err = ex.Apply(context.Background(), raw, digest, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	values, _ := db.SetupExecutionResults(context.Background(), plan.PlanID)
	if adapter.pushes != 1 || adapter.restoredHead == "" || len(values) != 2 || values[0].Attempt != 1 || values[1].Attempt != 2 {
		t.Fatalf("pushes=%d head=%q attempts=%#v", adapter.pushes, adapter.restoredHead, values)
	}
}

type pullAfterSuccessAdapter struct {
	created         bool
	creates, pushes int
	fail            bool
	oid             string
}

func (a *pullAfterSuccessAdapter) Readback(_ context.Context, e setupcontract.Effect) (setupcontract.EffectStatus, string, error) {
	if e.Kind == "repository_contract_pr" && !a.created {
		return setupcontract.EffectRequired, "missing", nil
	}
	return setupcontract.EffectSatisfied, "exact-pr-" + a.oid, nil
}
func (a *pullAfterSuccessAdapter) Apply(_ context.Context, e setupcontract.Effect) error {
	if e.Kind == "repository_contract_pr" {
		a.pushes++
		a.creates++
		a.created = true
		a.oid = strings.Repeat("c", 40)
		if a.fail {
			a.fail = false
			return errors.New("injected after PR create")
		}
	}
	return nil
}
func TestExecutorRetriesCreatedPullWithoutDuplicateBranchOrPR(t *testing.T) {
	home, repo := t.TempDir(), t.TempDir()
	db, err := store.Open(context.Background(), filepath.Join(home, "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	plan := setupcontract.Plan{SchemaVersion: 1, PlanID: "onboard-pr-retry", Kind: setupcontract.RepositoryOnboarding, Target: setupcontract.Target{WorkflowHome: home, RepositoryPath: repo, GitHubRepository: "owner/repo"}, Preconditions: []setupcontract.Precondition{{ID: "release", Kind: "platform_release", Subject: home, Expected: strings.Repeat("a", 64)}}, Effects: []setupcontract.Effect{{ID: "pr", Kind: "repository_contract_pr", Subject: "owner/repo", Action: "create_check_merge", Parameters: map[string]string{"base_branch": "main", "base_head": strings.Repeat("b", 40), "source_url": "https://github.com/owner/repo.git", "before_files_json": "{}", "files_json": "{\"AGENTS.md\":\"bWFuYWdlZAo=\"}", "manifest_digest": strings.Repeat("b", 64), "required_checks_json": "[{\"context\":\"workflow-contract\",\"app_id\":15368}]", "merge_method": "squash"}}, {ID: "admission", Kind: "repository_admission", Subject: "owner/repo", Action: "verify_and_record", Parameters: map[string]string{"default_branch": "main", "manifest_digest": strings.Repeat("b", 64), "contract_version": "1"}}}, ExpectedResults: []setupcontract.ExpectedResult{{ID: "ready", Kind: "repository_admission", Subject: "owner/repo", Expected: strings.Repeat("b", 64)}}}
	raw, _ := json.Marshal(plan)
	_, _, digest, err := setupcontract.ParsePlan(raw)
	if err != nil {
		t.Fatal(err)
	}
	a := &pullAfterSuccessAdapter{fail: true}
	x := Executor{Store: db, Adapter: a}
	if _, err = x.Apply(context.Background(), raw, digest, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err = x.Apply(context.Background(), raw, digest, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	r, _ := db.SetupExecutionResults(context.Background(), plan.PlanID)
	if a.creates != 1 || a.pushes != 1 || a.oid == "" || len(r) != 2 || r[0].Attempt != 1 || r[1].Attempt != 2 {
		t.Fatalf("creates=%d pushes=%d oid=%q attempts=%#v", a.creates, a.pushes, a.oid, r)
	}
}

type satisfiedAdapter struct{}

func (satisfiedAdapter) Readback(context.Context, setupcontract.Effect) (setupcontract.EffectStatus, string, error) {
	return setupcontract.EffectSatisfied, "verified", nil
}
func (satisfiedAdapter) Apply(context.Context, setupcontract.Effect) error { return nil }

func TestExecutorRecordsOnlyExactOnboardingDigestInGenerationStore(t *testing.T) {
	db, err := store.Open(context.Background(), t.TempDir()+"/workflow.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	raw := []byte(`{"schema_version":1,"plan_id":"onboard-001","kind":"repository_onboarding","target":{"workflow_home":"C:\\Workflow","repository_path":"C:\\repo","github_repository":"owner/repo"},"preconditions":[{"id":"release","kind":"platform_release","subject":"C:\\Workflow","expected":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},{"id":"head","kind":"git_head","subject":"C:\\repo","expected":""}],"effects":[{"id":"admission","kind":"repository_admission","subject":"owner/repo","action":"verify_and_record","parameters":{"default_branch":"main","manifest_digest":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","contract_version":"1"}}],"expected_results":[{"id":"ready","kind":"repository_admission","subject":"owner/repo","expected":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}]}`)
	_, _, digest, err := setupcontract.ParsePlan(raw)
	if err != nil {
		t.Fatal(err)
	}
	executor := Executor{Store: db, Adapter: satisfiedAdapter{}, Now: func() time.Time { return time.Unix(1, 0).UTC() }}
	if _, err := executor.Apply(context.Background(), raw, digest, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	if _, err := executor.Apply(context.Background(), raw, "different", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); err == nil {
		t.Fatal("accepted a non-exact onboarding digest")
	}
	if _, err := db.SetupPlan(context.Background(), "onboard-001"); err != nil {
		t.Fatal(err)
	}
}
