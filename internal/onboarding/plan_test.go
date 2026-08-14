package onboarding

import (
	"context"
	"encoding/json"
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

func TestPlanPublishedRepositoryContainsExactContractEffects(t *testing.T) {
	repo := newRepo(t)
	head := testGitOutput(t, repo, "rev-parse", "HEAD")
	plan, err := Plan(context.Background(), PlanOptions{RepositoryPath: repo, WorkflowHome: filepath.Join(t.TempDir(), "home"), Owner: "owner", AuthenticatedLogin: "owner", Remote: StaticRemoteHead{DefaultBranch: "main", Head: head}, PlatformReleaseDigest: repeatString("a", 64)})
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
	plan, err := Plan(context.Background(), PlanOptions{RepositoryPath: repo, WorkflowHome: filepath.Join(t.TempDir(), "home"), Owner: "owner", AuthenticatedLogin: "owner", PlatformReleaseDigest: repeatString("b", 64)})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, effect := range plan.Effects {
		kinds[effect.Kind] = true
	}
	if !kinds["create_repository"] || !kinds["initial_baseline"] || !kinds["repository_contract_pr"] {
		t.Fatalf("effects=%#v", plan.Effects)
	}
}

func TestPlanDeclaresFeatureEnablementAndAllRequiredChecksFromDiscoveredPolicy(t *testing.T) {
	repo := newRepo(t)
	head := testGitOutput(t, repo, "rev-parse", "HEAD")
	policy := policyDiscoveryFunc(func(_ context.Context, repository, branch string) (RepositoryPolicy, error) {
		if repository != "owner/repo" || branch != "main" {
			t.Fatalf("policy target=%s branch=%s", repository, branch)
		}
		return RepositoryPolicy{Admin: true, AllowSquashMerge: true, RequiredChecks: []string{"build"}}, nil
	})
	plan, err := Plan(context.Background(), PlanOptions{RepositoryPath: repo, WorkflowHome: filepath.Join(t.TempDir(), "home"), Owner: "owner", Remote: StaticRemoteHead{DefaultBranch: "main", Head: head}, PlatformReleaseDigest: repeatString("c", 64), Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	var features, contract bool
	for _, effect := range plan.Effects {
		switch effect.Kind {
		case "repository_features":
			features = true
		case "repository_contract_pr":
			contract = effect.Parameters["required_checks_json"] == `["workflow-contract","build"]`
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

func TestPlanHonorsExplicitPublicVisibility(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "public-repo")
	git(t, "", "init", "-b", "main", repo)
	public := false
	plan, err := Plan(context.Background(), PlanOptions{RepositoryPath: repo, WorkflowHome: filepath.Join(t.TempDir(), "home"), Owner: "owner", AuthenticatedLogin: "owner", Private: &public, PlatformReleaseDigest: repeatString("e", 64)})
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
