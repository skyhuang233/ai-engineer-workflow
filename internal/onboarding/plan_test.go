package onboarding

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skyhuang233/workflow/internal/setupcontract"
)

type policyDiscoveryFunc func(context.Context, string, string) (RepositoryPolicy, error)

func (f policyDiscoveryFunc) DiscoverPolicy(ctx context.Context, repository, branch string) (RepositoryPolicy, error) {
	return f(ctx, repository, branch)
}

type publicationPreflightFunc func(context.Context, string, string, string, bool) error

func (f publicationPreflightFunc) PreflightCreateRepository(ctx context.Context, owner, login, name string, private bool) error {
	return f(ctx, owner, login, name, private)
}

type onboardingStateFunc func(context.Context, string, string, string, []Label) (OnboardingState, error)

func (f onboardingStateFunc) DiscoverOnboardingState(ctx context.Context, repository, branch, manifestDigest string, labels []Label) (OnboardingState, error) {
	return f(ctx, repository, branch, manifestDigest, labels)
}

func approvedPublishedPolicy() PolicyDiscovery {
	return policyDiscoveryFunc(func(context.Context, string, string) (RepositoryPolicy, error) {
		return RepositoryPolicy{HasIssues: true, ActionsEnabled: true, ActionsAllowed: "all", GitHubOwnedActionsAllowed: true, Admin: true, AllowSquashMerge: true}, nil
	})
}

func approvedPublication() PublicationPreflight {
	return publicationPreflightFunc(func(context.Context, string, string, string, bool) error { return nil })
}

func TestPlanRequiresVerifiedRepositoryAbsenceBeforeApprovingCreation(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "new-repo")
	git(t, "", "init", "-b", "main", repo)
	_, err := Plan(context.Background(), PlanOptions{RepositoryPath: repo, WorkflowHome: filepath.Join(t.TempDir(), "home"), Owner: "owner", AuthenticatedLogin: "owner", PlatformReleaseDigest: repeatString("a", 64)})
	if err == nil || !strings.Contains(err.Error(), "absence preflight") {
		t.Fatalf("repository creation approved without verified absence: %v", err)
	}
}

func TestPlanPublishedRepositoryContainsExactContractEffects(t *testing.T) {
	repo := newRepo(t)
	head := testGitOutput(t, repo, "rev-parse", "HEAD")
	plan, err := Plan(context.Background(), PlanOptions{RepositoryPath: repo, WorkflowHome: filepath.Join(t.TempDir(), "home"), Owner: "owner", AuthenticatedLogin: "owner", Remote: StaticRemoteHead{DefaultBranch: "main", Head: head}, PlatformReleaseDigest: repeatString("a", 64), Policy: approvedPublishedPolicy()})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Kind != setupcontract.RepositoryOnboarding || plan.Target.GitHubRepository != "owner/repo" {
		t.Fatalf("plan=%#v", plan)
	}
	raw, _ := json.Marshal(plan)
	if _, _, _, err := setupcontract.ParsePlan(raw); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, effect := range plan.Effects {
		if effect.Kind == "repository_contract_pr" {
			found = true
			if effect.Parameters["files_json"] == "" || effect.Parameters["manifest_digest"] == "" {
				t.Fatalf("effect=%#v", effect)
			}
		}
	}
	if !found {
		t.Fatal("contract PR effect missing")
	}
}
func TestPlanUnpublishedZeroCommitDeclaresBaselineAndRepositoryCreation(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "new-repo")
	git(t, "", "init", "-b", "main", repo)
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := Plan(context.Background(), PlanOptions{RepositoryPath: repo, WorkflowHome: filepath.Join(t.TempDir(), "home"), Owner: "owner", AuthenticatedLogin: "owner", PlatformReleaseDigest: repeatString("b", 64), Publication: approvedPublication()})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	var baseline, contract setupcontract.Effect
	for _, effect := range plan.Effects {
		kinds[effect.Kind] = true
		if effect.Kind == "create_repository" && effect.Parameters["approval_absent_repository"] != "owner/new-repo" {
			t.Fatalf("repository creation does not bind approval-time absence identity: %#v", effect)
		}
		if effect.Kind == "initial_baseline" {
			baseline = effect
		}
		if effect.Kind == "repository_contract_pr" {
			contract = effect
		}
		if effect.Kind == "local_fast_forward" && effect.Parameters["merge_head_effect_id"] != "repository-contract-pr" {
			t.Fatalf("local fast-forward is not bound to merge evidence: %#v", effect)
		}
	}
	if !kinds["create_repository"] || !kinds["initial_baseline"] || !kinds["repository_contract_pr"] {
		t.Fatalf("effects=%#v", plan.Effects)
	}
	if contract.Parameters["base_head"] != "" || contract.Parameters["base_head_effect_id"] != baseline.ID {
		t.Fatalf("zero-commit contract base is not bound to Initial Repository Baseline evidence: baseline=%#v contract=%#v", baseline, contract)
	}
	raw, _ := json.Marshal(plan)
	if _, _, _, err := setupcontract.ParsePlan(raw); err != nil {
		t.Fatalf("zero-commit Onboarding Plan is not executable: %v", err)
	}
}

func TestNewRepositoryHistoryPublicationRequiresEarlierApprovedCreation(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "new-repo")
	git(t, "", "init", "-b", "main", repo)
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("history\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "README.md")
	git(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "base")
	plan, err := Plan(context.Background(), PlanOptions{RepositoryPath: repo, WorkflowHome: filepath.Join(t.TempDir(), "home"), Owner: "owner", AuthenticatedLogin: "owner", PlatformReleaseDigest: repeatString("b", 64), Publication: approvedPublication()})
	if err != nil {
		t.Fatal(err)
	}
	withoutCreate := plan
	withoutCreate.Effects = nil
	for _, effect := range plan.Effects {
		if effect.Kind != "create_repository" {
			withoutCreate.Effects = append(withoutCreate.Effects, effect)
		}
	}
	raw, _ := json.Marshal(withoutCreate)
	if _, _, _, err := setupcontract.ParsePlan(raw); err == nil || !strings.Contains(err.Error(), "without an earlier approved creation") {
		t.Fatalf("unverified new-repository publication accepted: %v", err)
	}
}

func TestPlanZeroCommitBindsBaselineContentAndPreservesExistingAgentsBytes(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "new-repo")
	git(t, "", "init", "-b", "main", repo)
	agents := []byte("# Existing instructions\n\nKeep this byte-for-byte.\n")
	if err := os.WriteFile(filepath.Join(repo, "AGENTS.md"), agents, 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := Plan(context.Background(), PlanOptions{RepositoryPath: repo, WorkflowHome: filepath.Join(t.TempDir(), "home"), Owner: "owner", AuthenticatedLogin: "owner", PlatformReleaseDigest: repeatString("b", 64), Publication: approvedPublication()})
	if err != nil {
		t.Fatal(err)
	}
	var baseline, contract setupcontract.Effect
	for _, effect := range plan.Effects {
		switch effect.Kind {
		case "initial_baseline":
			baseline = effect
		case "repository_contract_pr":
			contract = effect
		}
	}
	var snapshot []BaselineFile
	if err := json.Unmarshal([]byte(baseline.Parameters["files_json"]), &snapshot); err != nil || len(snapshot) != 1 || snapshot[0].Path != "AGENTS.md" || snapshot[0].SHA256 == "" {
		t.Fatalf("baseline snapshot = %#v, err=%v", snapshot, err)
	}
	var encoded map[string]string
	if err := json.Unmarshal([]byte(contract.Parameters["files_json"]), &encoded); err != nil {
		t.Fatal(err)
	}
	proposed, err := base64.StdEncoding.DecodeString(encoded["AGENTS.md"])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(proposed), string(agents)) || !strings.Contains(string(proposed), ManagedBlockStart) {
		t.Fatalf("proposed AGENTS.md did not preserve zero-commit bytes:\n%s", proposed)
	}
}

func TestPlanApprovalSnapshotRejectsZeroBaselineDriftBeforeEffects(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "new-repo")
	git(t, "", "init", "-b", "main", repo)
	path := filepath.Join(repo, "README.md")
	if err := os.WriteFile(path, []byte("approved\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := Plan(context.Background(), PlanOptions{RepositoryPath: repo, WorkflowHome: filepath.Join(t.TempDir(), "home"), Owner: "owner", AuthenticatedLogin: "owner", PlatformReleaseDigest: repeatString("b", 64), Publication: approvedPublication()})
	if err != nil {
		t.Fatal(err)
	}
	var snapshot setupcontract.Precondition
	for _, precondition := range plan.Preconditions {
		if precondition.Kind == "onboarding_snapshot" {
			snapshot = precondition
		}
	}
	if snapshot.Expected == "" || !strings.Contains(snapshot.Expected, `"mode":"100644"`) || !strings.Contains(snapshot.Expected, `"status_sha256"`) || !strings.Contains(snapshot.Expected, `"managed_boundary_sha256"`) {
		t.Fatalf("approval snapshot = %#v", snapshot)
	}
	if err := VerifyApprovalSnapshot(context.Background(), snapshot.Expected); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyApprovalSnapshot(context.Background(), snapshot.Expected); err == nil || !strings.Contains(err.Error(), "drifted") {
		t.Fatalf("zero-baseline drift accepted: %v", err)
	}
}

func TestPlanUsesCanonicalHTTPSForSSHOriginAndBindsRemoteBase(t *testing.T) {
	repo := newRepo(t)
	git(t, repo, "remote", "set-url", "origin", "git@github.com:owner/repo.git")
	head := testGitOutput(t, repo, "rev-parse", "HEAD")
	var resolvedURL string
	remote := remoteHeadFunc(func(_ context.Context, value string) (string, string, error) {
		resolvedURL = value
		return "main", head, nil
	})
	plan, err := Plan(context.Background(), PlanOptions{RepositoryPath: repo, WorkflowHome: filepath.Join(t.TempDir(), "home"), Owner: "owner", Remote: remote, PlatformReleaseDigest: repeatString("c", 64), Policy: approvedPublishedPolicy()})
	if err != nil {
		t.Fatal(err)
	}
	if resolvedURL != "https://github.com/owner/repo.git" {
		t.Fatalf("remote discovery URL = %q", resolvedURL)
	}
	if origin := testGitOutput(t, repo, "remote", "get-url", "origin"); origin != "git@github.com:owner/repo.git" {
		t.Fatalf("SSH origin was rewritten to %q", origin)
	}
	var foundPrecondition bool
	for _, precondition := range plan.Preconditions {
		if precondition.Kind == "github_default_head" && strings.Contains(precondition.Expected, head) && strings.Contains(precondition.Expected, "main") {
			foundPrecondition = true
		}
	}
	for _, effect := range plan.Effects {
		if effect.Kind == "repository_contract_pr" && effect.Parameters["source_url"] != "https://github.com/owner/repo.git" {
			t.Fatalf("contract source URL = %q", effect.Parameters["source_url"])
		}
	}
	if !foundPrecondition {
		t.Fatalf("remote default-head precondition missing: %#v", plan.Preconditions)
	}
}

type remoteHeadFunc func(context.Context, string) (string, string, error)

func (f remoteHeadFunc) Resolve(ctx context.Context, origin string) (string, string, error) {
	return f(ctx, origin)
}

func TestPlanDeclaresFeatureEnablementAndAllRequiredChecksFromDiscoveredPolicy(t *testing.T) {
	repo := newRepo(t)
	head := testGitOutput(t, repo, "rev-parse", "HEAD")
	policy := policyDiscoveryFunc(func(_ context.Context, repository, branch string) (RepositoryPolicy, error) {
		if repository != "owner/repo" || branch != "main" {
			t.Fatalf("policy target=%s branch=%s", repository, branch)
		}
		return RepositoryPolicy{Admin: true, AllowSquashMerge: true, ActionsAllowed: "selected", GitHubOwnedActionsAllowed: true, RequiredChecks: []RequiredCheck{{Context: "build", AppID: 42}}}, nil
	})
	plan, err := Plan(context.Background(), PlanOptions{RepositoryPath: repo, WorkflowHome: filepath.Join(t.TempDir(), "home"), Owner: "owner", Remote: StaticRemoteHead{DefaultBranch: "main", Head: head}, PlatformReleaseDigest: repeatString("c", 64), Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	var features, contract bool
	for _, effect := range plan.Effects {
		switch effect.Kind {
		case "repository_features":
			features = effect.Parameters["allowed_actions"] == "selected"
		case "repository_contract_pr":
			contract = effect.Parameters["required_checks_json"] == `[{"context":"workflow-contract","app_id":15368},{"context":"build","app_id":42}]`
		}
	}
	if !features || !contract {
		t.Fatalf("effects=%#v", plan.Effects)
	}
}

func TestPlanBlocksRepositoryPolicyThatNeedsHumanReview(t *testing.T) {
	repo := newRepo(t)
	head := testGitOutput(t, repo, "rev-parse", "HEAD")
	policy := policyDiscoveryFunc(func(context.Context, string, string) (RepositoryPolicy, error) {
		return RepositoryPolicy{HasIssues: true, ActionsEnabled: true, AllowSquashMerge: true, RequiredHumanReviews: true}, nil
	})
	_, err := Plan(context.Background(), PlanOptions{RepositoryPath: repo, WorkflowHome: filepath.Join(t.TempDir(), "home"), Owner: "owner", Remote: StaticRemoteHead{DefaultBranch: "main", Head: head}, PlatformReleaseDigest: repeatString("d", 64), Policy: policy})
	if err == nil || !strings.Contains(err.Error(), "human review") {
		t.Fatalf("err=%v", err)
	}
}

func TestPlanFailsClosedWhenRequiredCheckLacksAppIdentity(t *testing.T) {
	repo := newRepo(t)
	head := testGitOutput(t, repo, "rev-parse", "HEAD")
	policy := policyDiscoveryFunc(func(context.Context, string, string) (RepositoryPolicy, error) {
		return RepositoryPolicy{HasIssues: true, ActionsEnabled: true, ActionsAllowed: "all", GitHubOwnedActionsAllowed: true, Admin: true, AllowSquashMerge: true, RequiredChecks: []RequiredCheck{{Context: "build"}}}, nil
	})
	_, err := Plan(context.Background(), PlanOptions{RepositoryPath: repo, WorkflowHome: filepath.Join(t.TempDir(), "home"), Owner: "owner", Remote: StaticRemoteHead{DefaultBranch: "main", Head: head}, PlatformReleaseDigest: repeatString("d", 64), Policy: policy})
	if err == nil || !strings.Contains(err.Error(), "App identity") {
		t.Fatalf("unbound required check accepted: %v", err)
	}
}

func TestPlanBlocksPublishedRepositoryWithoutVerifiedAdministration(t *testing.T) {
	repo := newRepo(t)
	head := testGitOutput(t, repo, "rev-parse", "HEAD")
	policy := policyDiscoveryFunc(func(context.Context, string, string) (RepositoryPolicy, error) {
		return RepositoryPolicy{HasIssues: true, ActionsEnabled: true, ActionsAllowed: "all", GitHubOwnedActionsAllowed: true, AllowSquashMerge: true}, nil
	})
	_, err := Plan(context.Background(), PlanOptions{RepositoryPath: repo, WorkflowHome: filepath.Join(t.TempDir(), "home"), Owner: "owner", Remote: StaticRemoteHead{DefaultBranch: "main", Head: head}, PlatformReleaseDigest: repeatString("d", 64), Policy: policy})
	if err == nil || !strings.Contains(err.Error(), "administration") {
		t.Fatalf("published PAT preflight = %v", err)
	}
}

func TestPlanHonorsExplicitPublicVisibility(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "public-repo")
	git(t, "", "init", "-b", "main", repo)
	public := false
	plan, err := Plan(context.Background(), PlanOptions{RepositoryPath: repo, WorkflowHome: filepath.Join(t.TempDir(), "home"), Owner: "owner", AuthenticatedLogin: "owner", Private: &public, PlatformReleaseDigest: repeatString("e", 64), Publication: approvedPublication()})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Effects[0].Parameters["private"] != "false" {
		t.Fatalf("effect=%#v", plan.Effects[0])
	}
}

func TestPlanRejectsPublishedRepositoryUnderDifferentOwner(t *testing.T) {
	repo := newRepo(t)
	head := testGitOutput(t, repo, "rev-parse", "HEAD")
	_, err := Plan(context.Background(), PlanOptions{RepositoryPath: repo, WorkflowHome: filepath.Join(t.TempDir(), "home"), Owner: "different-owner", Remote: StaticRemoteHead{DefaultBranch: "main", Head: head}, PlatformReleaseDigest: repeatString("f", 64)})
	if err == nil || !strings.Contains(err.Error(), "owner differs") {
		t.Fatalf("err=%v", err)
	}
}

func TestPlanPreflightsUnpublishedOrganizationBeforeAuthorizingFirstMutation(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "org-repo")
	git(t, "", "init", "-b", "main", repo)
	called := false
	preflight := publicationPreflightFunc(func(_ context.Context, owner, login, name string, private bool) error {
		called = owner == "acme" && login == "alice" && name == "org-repo" && private
		return errors.New("organization repository creation is forbidden")
	})
	_, err := Plan(context.Background(), PlanOptions{RepositoryPath: repo, WorkflowHome: filepath.Join(t.TempDir(), "home"), Owner: "acme", AuthenticatedLogin: "alice", PlatformReleaseDigest: repeatString("a", 64), Publication: preflight})
	if err == nil || !called || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("organization preflight result: called=%v err=%v", called, err)
	}
}

func TestPlanContainsOnlyUnsatisfiedOnboardingDeltas(t *testing.T) {
	repo := newRepo(t)
	head := testGitOutput(t, repo, "rev-parse", "HEAD")
	labels := []Label{{Name: "workflow:plan", Color: "123456", Description: "plan"}, {Name: "workflow:ticket", Color: "abcdef", Description: "ticket"}}
	state := onboardingStateFunc(func(_ context.Context, repository, branch, manifest string, desired []Label) (OnboardingState, error) {
		if repository != "owner/repo" || branch != "main" || manifest == "" || len(desired) != 2 {
			t.Fatalf("state discovery inputs repository=%q branch=%q manifest=%q labels=%#v", repository, branch, manifest, desired)
		}
		return OnboardingState{SatisfiedLabels: map[string]bool{"workflow:plan": true}, ContractSatisfied: true, AdmissionSatisfied: true}, nil
	})
	policy := policyDiscoveryFunc(func(context.Context, string, string) (RepositoryPolicy, error) {
		return RepositoryPolicy{HasIssues: true, ActionsEnabled: true, ActionsAllowed: "selected", GitHubOwnedActionsAllowed: true, Admin: true, AllowSquashMerge: true}, nil
	})
	plan, err := Plan(context.Background(), PlanOptions{RepositoryPath: repo, WorkflowHome: filepath.Join(t.TempDir(), "home"), Owner: "owner", AuthenticatedLogin: "owner", Remote: StaticRemoteHead{DefaultBranch: "main", Head: head}, PlatformReleaseDigest: repeatString("b", 64), Labels: labels, Policy: policy, State: state})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Effects) != 2 || plan.Effects[0].Kind != "github_label" || plan.Effects[0].Parameters["name"] != "workflow:ticket" || plan.Effects[1].Kind != "repository_admission" {
		t.Fatalf("non-delta effects = %#v", plan.Effects)
	}
	raw, _ := json.Marshal(plan)
	if _, _, _, err := setupcontract.ParsePlan(raw); err != nil {
		t.Fatalf("label-only repair plan is not executable: %v", err)
	}
}

func TestPlanAlwaysReverifiesAdmissionForFeatureOnlyRepair(t *testing.T) {
	repo := newRepo(t)
	head := testGitOutput(t, repo, "rev-parse", "HEAD")
	state := onboardingStateFunc(func(context.Context, string, string, string, []Label) (OnboardingState, error) {
		return OnboardingState{ContractSatisfied: true, AdmissionSatisfied: true}, nil
	})
	policy := policyDiscoveryFunc(func(context.Context, string, string) (RepositoryPolicy, error) {
		return RepositoryPolicy{HasIssues: false, ActionsEnabled: true, ActionsAllowed: "all", GitHubOwnedActionsAllowed: true, Admin: true, AllowSquashMerge: true}, nil
	})
	plan, err := Plan(context.Background(), PlanOptions{RepositoryPath: repo, WorkflowHome: filepath.Join(t.TempDir(), "home"), Owner: "owner", AuthenticatedLogin: "owner", Remote: StaticRemoteHead{DefaultBranch: "main", Head: head}, PlatformReleaseDigest: repeatString("b", 64), Policy: policy, State: state})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, effect := range plan.Effects {
		kinds[effect.Kind] = true
	}
	if !kinds["repository_features"] || !kinds["repository_admission"] {
		t.Fatalf("feature repair lacks admission reverification: %#v", plan.Effects)
	}
	raw, _ := json.Marshal(plan)
	if _, _, _, err := setupcontract.ParsePlan(raw); err != nil {
		t.Fatal(err)
	}
}

func TestPlanCreatesForwardRepairForManagedContractDrift(t *testing.T) {
	repo := newRepo(t)
	head := testGitOutput(t, repo, "rev-parse", "HEAD")
	state := onboardingStateFunc(func(context.Context, string, string, string, []Label) (OnboardingState, error) {
		return OnboardingState{ContractSatisfied: false, AdmissionSatisfied: true}, nil
	})
	policy := policyDiscoveryFunc(func(context.Context, string, string) (RepositoryPolicy, error) {
		return RepositoryPolicy{HasIssues: true, ActionsEnabled: true, ActionsAllowed: "all", GitHubOwnedActionsAllowed: true, Admin: true, AllowSquashMerge: true}, nil
	})
	plan, err := Plan(context.Background(), PlanOptions{RepositoryPath: repo, WorkflowHome: filepath.Join(t.TempDir(), "home"), Owner: "owner", AuthenticatedLogin: "owner", Remote: StaticRemoteHead{DefaultBranch: "main", Head: head}, PlatformReleaseDigest: repeatString("c", 64), Policy: policy, State: state})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, effect := range plan.Effects {
		kinds[effect.Kind] = true
	}
	if !kinds["repository_contract_pr"] || !kinds["local_fast_forward"] || !kinds["repository_admission"] {
		t.Fatalf("forward-repair effects = %#v", plan.Effects)
	}
	raw, _ := json.Marshal(plan)
	if _, _, _, err := setupcontract.ParsePlan(raw); err != nil {
		t.Fatalf("forward-repair plan is not executable: %v", err)
	}
}
