package setup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skyhuang233/workflow/internal/controlplane"
	"github.com/skyhuang233/workflow/internal/credential"
	workflowgithub "github.com/skyhuang233/workflow/internal/github"
	"github.com/skyhuang233/workflow/internal/githubcredential"
	"github.com/skyhuang233/workflow/internal/onboarding"
	"github.com/skyhuang233/workflow/internal/platformrelease"
	"github.com/skyhuang233/workflow/internal/repositorycontract"
	"github.com/skyhuang233/workflow/internal/setupcontract"
	"github.com/skyhuang233/workflow/internal/store"
	"github.com/skyhuang233/workflow/internal/workflowhome"
)

func TestHostAdapterChecksApprovedCurrentUserIdentity(t *testing.T) {
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	ownerID, err := workflowHomeOwnerIdentity(root)
	if err != nil {
		t.Fatal(err)
	}
	expected, _ := json.Marshal(hostIdentityPrecondition{UserID: current.Uid, Username: current.Username, WorkflowHome: root, WorkflowHomeOwnerID: ownerID})
	adapter := HostAdapter{Layout: workflowhome.Layout{Root: root}}
	if err := adapter.CheckPrecondition(context.Background(), setupcontract.Precondition{ID: "user", Kind: "host_identity", Subject: "current-user", Expected: string(expected)}); err != nil {
		t.Fatal(err)
	}
	wrongUser, _ := json.Marshal(hostIdentityPrecondition{UserID: "different-user", Username: current.Username, WorkflowHome: root, WorkflowHomeOwnerID: ownerID})
	if err := adapter.CheckPrecondition(context.Background(), setupcontract.Precondition{ID: "user", Kind: "host_identity", Subject: "current-user", Expected: string(wrongUser)}); err == nil {
		t.Fatal("accepted drifted host identity")
	}
	wrongOwner, _ := json.Marshal(hostIdentityPrecondition{UserID: current.Uid, Username: current.Username, WorkflowHome: root, WorkflowHomeOwnerID: "different-owner"})
	if err := adapter.CheckPrecondition(context.Background(), setupcontract.Precondition{ID: "user", Kind: "host_identity", Subject: "current-user", Expected: string(wrongOwner)}); err == nil {
		t.Fatal("accepted Workflow Home owned by another identity")
	}
}

func TestHostAdapterGitHeadPreconditionCannotBeRedirectedByProcessGitEnvironment(t *testing.T) {
	createRepository := func(name, contents string) (string, string) {
		repository := filepath.Join(t.TempDir(), name)
		hostGit(t, "", "init", "-b", "main", repository)
		hostGit(t, repository, "config", "user.name", "Test")
		hostGit(t, repository, "config", "user.email", "test@example.com")
		if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		hostGit(t, repository, "add", "README.md")
		hostGit(t, repository, "commit", "-m", name)
		return repository, hostGitOutput(t, repository, "rev-parse", "HEAD")
	}
	target, targetHead := createRepository("target", "target\n")
	attacker, attackerHead := createRepository("attacker", "attacker\n")
	if targetHead == attackerHead {
		t.Fatal("test repositories unexpectedly have the same HEAD")
	}
	t.Setenv("GIT_DIR", filepath.Join(attacker, ".git"))
	t.Setenv("GIT_WORK_TREE", attacker)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.worktree")
	t.Setenv("GIT_CONFIG_VALUE_0", attacker)

	err := (HostAdapter{}).CheckPrecondition(context.Background(), setupcontract.Precondition{
		ID: "head", Kind: "git_head", Subject: target, Expected: attackerHead,
	})
	if err == nil {
		t.Fatal("process Git environment redirected the approved repository precondition")
	}
}

func TestHostAdapterAllowsMatchingIdentityBeforeCreatingWorkflowHome(t *testing.T) {
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "fresh-workflow-home")
	expected, _ := json.Marshal(hostIdentityPrecondition{UserID: current.Uid, Username: current.Username, WorkflowHome: root, WorkflowHomeOwnerID: current.Uid})
	adapter := HostAdapter{Layout: workflowhome.Layout{Root: root}}
	precondition := setupcontract.Precondition{ID: "user", Kind: "host_identity", Subject: "current-user", Expected: string(expected)}
	if err := adapter.CheckPreLayoutPrecondition(context.Background(), precondition); err != nil {
		t.Fatalf("matching pre-layout host identity was rejected: %v", err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("identity preflight created Workflow Home: %v", err)
	}
}

func TestHostAdapterPreLayoutRejectsOwnerSnapshotNotBoundToExistingWorkflowHome(t *testing.T) {
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	actualOwner, err := workflowHomeOwnerIdentity(root)
	if err != nil {
		t.Fatal(err)
	}
	wrongOwner := "S-1-5-32-544"
	if strings.EqualFold(actualOwner, wrongOwner) {
		wrongOwner = "S-1-5-21-other-user"
	}
	expected, _ := json.Marshal(hostIdentityPrecondition{UserID: current.Uid, Username: current.Username, WorkflowHome: root, WorkflowHomeOwnerID: wrongOwner})
	adapter := HostAdapter{Layout: workflowhome.Layout{Root: root}}
	err = adapter.CheckPreLayoutPrecondition(context.Background(), setupcontract.Precondition{ID: "user", Kind: "host_identity", Subject: "current-user", Expected: string(expected)})
	if err == nil || !strings.Contains(err.Error(), "owner") {
		t.Fatalf("existing Workflow Home accepted an unbound owner snapshot: %v", err)
	}
}

func TestRepositoryContractReadbackRequiresRepairForManagedContentDrift(t *testing.T) {
	files, _, digest, err := repositorycontract.Render("single-context", []byte("# User instructions\n"), "owner/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		path string
	}{
		{name: "managed file", path: "docs/agents/domain.md"},
		{name: "managed AGENTS block", path: "AGENTS.md"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`[]`))
					return
				}
				if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, "/repos/owner/repo/contents/") {
					t.Fatalf("managed drift readback continued to %s %s", r.Method, r.URL.String())
				}
				path := strings.TrimPrefix(r.URL.Path, "/repos/owner/repo/contents/")
				content, ok := files[path]
				if !ok {
					t.Fatalf("unexpected repository file %q", path)
				}
				if path == test.path {
					content = []byte("drifted\n")
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]string{"encoding": "base64", "content": base64.StdEncoding.EncodeToString(content)})
			}))
			defer server.Close()

			adapter := HostAdapter{
				GitHub:               workflowgithub.NewClient(server.URL, "token", server.Client()),
				PlanDigest:           strings.Repeat("a", 64),
				OnboardingMergeHeads: map[string]string{},
			}
			effect := setupcontract.Effect{
				ID:      "repository-contract-pr",
				Kind:    "repository_contract_pr",
				Subject: "owner/repo",
				Action:  "create_check_merge",
				Parameters: map[string]string{
					"base_branch":          "main",
					"base_head":            strings.Repeat("b", 40),
					"source_url":           "https://github.com/owner/repo.git",
					"before_files_json":    "[]",
					"files_json":           "[]",
					"manifest_digest":      digest,
					"required_checks_json": `[{"context":"workflow-contract","app_id":15368}]`,
				},
			}
			status, evidence, err := adapter.Readback(context.Background(), effect)
			if err != nil || status != setupcontract.EffectRequired || !strings.Contains(strings.ToLower(evidence), "drift") {
				t.Fatalf("managed drift readback = %s, %q, %v", status, evidence, err)
			}
		})
	}
}

func TestRepositoryContractApplyPreservesPrimaryAndCleanupFailures(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	hostGit(t, "", "init", "-b", "main", source)
	hostGit(t, source, "config", "user.name", "Test")
	hostGit(t, source, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hostGit(t, source, "add", "README.md")
	hostGit(t, source, "commit", "-m", "base")
	base := hostGitOutput(t, source, "rev-parse", "HEAD")
	bare := filepath.Join(t.TempDir(), "remote.git")
	hostGit(t, "", "clone", "--bare", source, bare)

	deleted := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo":
			_, _ = w.Write([]byte(`{"default_branch":"main"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/git/ref/heads/main":
			_, _ = w.Write([]byte(`{"object":{"sha":"` + base + `"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls":
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && r.URL.Path == "/repos/owner/repo/pulls":
			_, _ = w.Write([]byte(`{"number":7}`))
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/repos/owner/repo/git/refs/heads/workflow/onboarding-"):
			deleted = true
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"remote branch cleanup denied"}`))
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	files, err := json.Marshal(map[string]string{"managed.txt": base64.StdEncoding.EncodeToString([]byte("contract\n"))})
	if err != nil {
		t.Fatal(err)
	}
	cleanupCalled := false
	adapter := HostAdapter{
		GitHub:               workflowgithub.NewClient(server.URL, "token", server.Client()).WithOnboardingIdentity("owner", "owner", "owner/repo"),
		PlanDigest:           strings.Repeat("a", 64),
		TemporaryRoot:        t.TempDir(),
		OnboardingMergeHeads: map[string]string{},
		CleanupOnboardingWorkspace: func(onboarding.GitWorkspace) error {
			cleanupCalled = true
			return errors.New("temporary clone cleanup failed")
		},
	}
	effect := setupcontract.Effect{ID: "repository-contract-pr", Kind: "repository_contract_pr", Subject: "owner/repo", Action: "create_check_merge", Parameters: map[string]string{
		"files_json": string(files), "source_url": bare, "base_head": base, "base_branch": "main", "required_checks_json": `[]`,
	}}
	err = adapter.applyRepositoryContract(context.Background(), effect)
	if err == nil {
		t.Fatal("repository contract apply reported success despite primary and cleanup failures")
	}
	for _, wanted := range []string{"lacks approved required checks", "remote branch cleanup denied", "temporary clone cleanup failed"} {
		if !strings.Contains(err.Error(), wanted) {
			t.Fatalf("combined error %q lacks %q", err, wanted)
		}
	}
	if !deleted || !cleanupCalled {
		t.Fatalf("cleanup attempts: remote=%t workspace=%t", deleted, cleanupCalled)
	}
}

func TestRepositoryContractApplyFailsWhenSuccessfulMutationCannotCleanUp(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	hostGit(t, "", "init", "-b", "main", source)
	hostGit(t, source, "config", "user.name", "Test")
	hostGit(t, source, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hostGit(t, source, "add", "README.md")
	hostGit(t, source, "commit", "-m", "base")
	base := hostGitOutput(t, source, "rev-parse", "HEAD")
	bare := filepath.Join(t.TempDir(), "remote.git")
	hostGit(t, "", "clone", "--bare", source, bare)
	branch := "workflow/onboarding-" + strings.Repeat("a", 12)
	mergeHead := strings.Repeat("c", 40)
	merged := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		workspaceHead := func() string {
			return hostGitOutput(t, "", "--git-dir", bare, "rev-parse", "refs/heads/"+branch)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo":
			_, _ = w.Write([]byte(`{"default_branch":"main","allow_squash_merge":true}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/actions/permissions":
			_, _ = w.Write([]byte(`{"enabled":true,"allowed_actions":"all"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/branches/main/protection":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"not found"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/rulesets":
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/git/ref/heads/main":
			head := base
			if merged {
				head = mergeHead
			}
			_, _ = w.Write([]byte(`{"object":{"sha":"` + head + `"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls":
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && r.URL.Path == "/repos/owner/repo/pulls":
			_, _ = w.Write([]byte(`{"number":7}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/check-runs"):
			_, _ = w.Write([]byte(`{"check_runs":[{"name":"workflow-contract","status":"completed","conclusion":"success","head_sha":"` + workspaceHead() + `","app":{"id":15368}}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls/7":
			_, _ = w.Write([]byte(`{"number":7,"mergeable":true,"head":{"sha":"` + workspaceHead() + `","ref":"` + branch + `"},"base":{"sha":"` + base + `","ref":"main"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls/7/reviews":
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPut && r.URL.Path == "/repos/owner/repo/pulls/7/merge":
			merged = true
			_, _ = w.Write([]byte(`{"merged":true,"sha":"` + mergeHead + `"}`))
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/repos/owner/repo/git/refs/heads/workflow/onboarding-"):
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"remote branch cleanup denied"}`))
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	files, err := json.Marshal(map[string]string{"managed.txt": base64.StdEncoding.EncodeToString([]byte("contract\n"))})
	if err != nil {
		t.Fatal(err)
	}
	adapter := HostAdapter{
		GitHub:               workflowgithub.NewClient(server.URL, "token", server.Client()).WithOnboardingIdentity("owner", "owner", "owner/repo"),
		PlanDigest:           strings.Repeat("a", 64),
		TemporaryRoot:        t.TempDir(),
		OnboardingMergeHeads: map[string]string{},
		ApprovedGitHubPolicies: map[string]string{"owner/repo": approvedPolicyJSON(t, onboarding.RepositoryPolicy{
			ActionsEnabled: true, ActionsAllowed: "all", GitHubOwnedActionsAllowed: true, AllowSquashMerge: true,
		})},
		CleanupOnboardingWorkspace: func(onboarding.GitWorkspace) error {
			return errors.New("temporary clone cleanup failed")
		},
	}
	effect := setupcontract.Effect{ID: "repository-contract-pr", Kind: "repository_contract_pr", Subject: "owner/repo", Action: "create_check_merge", Parameters: map[string]string{
		"files_json": string(files), "source_url": bare, "base_head": base, "base_branch": "main", "required_checks_json": `[{"context":"workflow-contract","app_id":15368}]`,
	}}
	err = adapter.applyRepositoryContract(context.Background(), effect)
	if err == nil {
		t.Fatal("repository contract apply reported success despite cleanup residue")
	}
	for _, wanted := range []string{"remote branch cleanup denied", "temporary clone cleanup failed"} {
		if !strings.Contains(err.Error(), wanted) {
			t.Fatalf("cleanup error %q lacks %q", err, wanted)
		}
	}
}

func TestRepositoryContractApplyRechecksApprovedPolicyImmediatelyBeforeMerge(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	hostGit(t, "", "init", "-b", "main", source)
	hostGit(t, source, "config", "user.name", "Test")
	hostGit(t, source, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hostGit(t, source, "add", "README.md")
	hostGit(t, source, "commit", "-m", "base")
	base := hostGitOutput(t, source, "rev-parse", "HEAD")
	bare := filepath.Join(t.TempDir(), "remote.git")
	hostGit(t, "", "clone", "--bare", source, bare)
	branch := "workflow/onboarding-" + strings.Repeat("b", 12)
	checks, reviews, merged := 0, 0, false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		workspaceHead := func() string { return hostGitOutput(t, "", "--git-dir", bare, "rev-parse", "refs/heads/"+branch) }
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo":
			// Issues were enabled by the approved repository-features transition.
			// Disabling them after approval must be caught after the second
			// checks/reviews read and before PUT.
			_, _ = w.Write([]byte(`{"default_branch":"main","allow_squash_merge":true,"has_issues":false}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/git/ref/heads/main":
			_, _ = w.Write([]byte(`{"object":{"sha":"` + base + `"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls":
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && r.URL.Path == "/repos/owner/repo/pulls":
			_, _ = w.Write([]byte(`{"number":7}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/check-runs"):
			checks++
			_, _ = w.Write([]byte(`{"check_runs":[{"name":"workflow-contract","status":"completed","conclusion":"success","head_sha":"` + workspaceHead() + `","app":{"id":15368}}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls/7":
			_, _ = w.Write([]byte(`{"number":7,"mergeable":true,"head":{"sha":"` + workspaceHead() + `","ref":"` + branch + `"},"base":{"sha":"` + base + `","ref":"main"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls/7/reviews":
			reviews++
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/actions/permissions":
			_, _ = w.Write([]byte(`{"enabled":true,"allowed_actions":"all"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/branches/main/protection":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"not found"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/rulesets":
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPut && r.URL.Path == "/repos/owner/repo/pulls/7/merge":
			merged = true
			_, _ = w.Write([]byte(`{"merged":true,"sha":"` + strings.Repeat("c", 40) + `"}`))
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/repos/owner/repo/git/refs/heads/workflow/onboarding-"):
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()
	files, _ := json.Marshal(map[string]string{"managed.txt": base64.StdEncoding.EncodeToString([]byte("contract\n"))})
	adapter := HostAdapter{
		GitHub: workflowgithub.NewClient(server.URL, "token", server.Client()).WithOnboardingIdentity("owner", "owner", "owner/repo"), PlanDigest: strings.Repeat("b", 64), TemporaryRoot: t.TempDir(), OnboardingMergeHeads: map[string]string{},
		ApprovedGitHubPolicies: map[string]string{"owner/repo": approvedPolicyJSON(t, onboarding.RepositoryPolicy{HasIssues: true, ActionsEnabled: true, ActionsAllowed: "all", GitHubOwnedActionsAllowed: true, AllowSquashMerge: true, AllowFeatureEnable: true})},
	}
	effect := setupcontract.Effect{ID: "repository-contract-pr", Kind: "repository_contract_pr", Subject: "owner/repo", Action: "create_check_merge", Parameters: map[string]string{
		"files_json": string(files), "source_url": bare, "base_head": base, "base_branch": "main", "required_checks_json": `[{"context":"workflow-contract","app_id":15368}]`,
	}}
	err := adapter.applyRepositoryContract(context.Background(), effect)
	if err == nil || !strings.Contains(err.Error(), "policy drifted") || merged || checks < 2 || reviews < 2 {
		t.Fatalf("policy drift result err=%v merged=%t checks=%d reviews=%d", err, merged, checks, reviews)
	}
}

func TestApprovedRepositoryPolicyComparisonDoesNotNormalizeLiveFeatureDrift(t *testing.T) {
	approved := onboarding.RepositoryPolicy{
		HasIssues: true, ActionsEnabled: true, ActionsAllowed: "all", GitHubOwnedActionsAllowed: true,
		AllowSquashMerge: true, AllowFeatureEnable: true,
	}
	actual := approved
	actual.HasIssues = false
	if err := compareApprovedRepositoryPolicy(actual, approved); err == nil {
		t.Fatal("live issue disablement was normalized to the approved post-transition value")
	}
	actual = approved
	actual.ActionsEnabled = false
	if err := compareApprovedRepositoryPolicy(actual, approved); err == nil {
		t.Fatal("live Actions disablement was normalized to the approved post-transition value")
	}
	approved.RequiredChecks = []onboarding.RequiredCheck{{Context: "build", AppID: 7}, {Context: "test", AppID: 9}}
	actual = approved
	actual.RequiredChecks = []onboarding.RequiredCheck{{Context: "test", AppID: 9}, {Context: "build", AppID: 7}, {Context: "build", AppID: 7}}
	if err := compareApprovedRepositoryPolicy(actual, approved); err != nil {
		t.Fatalf("required check response order caused false policy drift: %v", err)
	}
}

func TestOnboardingCheckWaiterIgnoresOptionalFailuresAndPendingRuns(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/check-runs") {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.String())
		}
		_, _ = w.Write([]byte(`{"check_runs":[{"name":"workflow-contract","status":"completed","conclusion":"success","head_sha":"` + strings.Repeat("a", 40) + `","app":{"id":15368}},{"name":"optional-lint","status":"completed","conclusion":"failure","app":{"id":99}},{"name":"optional-preview","status":"in_progress","conclusion":"","app":{"id":99}}]}`))
	}))
	defer server.Close()
	diagnostics, err := waitForOnboardingChecks(context.Background(), workflowgithub.NewClient(server.URL, "token", server.Client()), "owner/repo", strings.Repeat("a", 40), []onboarding.RequiredCheck{{Context: "workflow-contract", AppID: 15368}}, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("optional checks blocked onboarding: %v", err)
	}
	joined := strings.Join(diagnostics, "\n")
	if !strings.Contains(joined, "optional-lint") || !strings.Contains(joined, "failure") || !strings.Contains(joined, "optional-preview") || !strings.Contains(joined, "in_progress") {
		t.Fatalf("optional check diagnostics = %#v", diagnostics)
	}
}

func TestOnboardingCheckWaiterTimeoutReportsRequiredObservedState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"check_runs":[{"name":"workflow-contract","status":"in_progress","conclusion":"","head_sha":"` + strings.Repeat("a", 40) + `","app":{"id":15368}}]}`))
	}))
	defer server.Close()
	_, err := waitForOnboardingChecks(context.Background(), workflowgithub.NewClient(server.URL, "token", server.Client()), "owner/repo", strings.Repeat("a", 40), []onboarding.RequiredCheck{{Context: "workflow-contract", AppID: 15368}, {Context: "build", AppID: 42}}, 10*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "workflow-contract (status=in_progress") || !strings.Contains(err.Error(), "build (not observed)") {
		t.Fatalf("required-check timeout diagnostics = %v", err)
	}
}

func TestOnboardingCheckWaiterRejectsSameNameFromDifferentApp(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"check_runs":[{"name":"workflow-contract","status":"completed","conclusion":"success","head_sha":"` + strings.Repeat("a", 40) + `","app":{"id":999}}]}`))
	}))
	defer server.Close()
	diagnostics, err := waitForOnboardingChecks(context.Background(), workflowgithub.NewClient(server.URL, "token", server.Client()), "owner/repo", strings.Repeat("a", 40), []onboarding.RequiredCheck{{Context: "workflow-contract", AppID: 15368}}, 10*time.Millisecond)
	if err == nil || !strings.Contains(strings.Join(diagnostics, "\n"), "unapproved App 999") {
		t.Fatalf("same-name check from another App was accepted: diagnostics=%#v err=%v", diagnostics, err)
	}
}

func TestPublishHistoryTreatsRefConflictAsRequiredOnlyForApprovedNewRepository(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/owner/repo":
			_, _ = w.Write([]byte(`{"default_branch":"main"}`))
		case "/repos/owner/repo/git/ref/heads/main":
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"message":"Git Repository is empty."}`))
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()
	adapter := HostAdapter{GitHub: workflowgithub.NewClient(server.URL, "token", server.Client()), CreatedRepositories: map[string]bool{}}
	for _, test := range []struct {
		name, newlyCreated string
		createdEvidence    bool
		want               setupcontract.EffectStatus
	}{
		{name: "plan flag without evidence", newlyCreated: "true", want: setupcontract.EffectFailed},
		{name: "existing repository", newlyCreated: "false", want: setupcontract.EffectFailed},
		{name: "verified created", newlyCreated: "true", createdEvidence: true, want: setupcontract.EffectRequired},
	} {
		t.Run(test.name, func(t *testing.T) {
			adapter.CreatedRepositories["owner/repo"] = test.createdEvidence
			effect := setupcontract.Effect{ID: "publish", Kind: "publish_history", Subject: "owner/repo", Action: "push", Parameters: map[string]string{"branch": "main", "head": strings.Repeat("a", 40), "new_repository": test.newlyCreated}}
			status, _, err := adapter.Readback(context.Background(), effect)
			if status != test.want || test.want == setupcontract.EffectFailed && err == nil {
				t.Fatalf("409 readback = %s, %v; want %s", status, err, test.want)
			}
		})
	}
}

func TestInitialBaselineTreatsEmptyRemoteConflictAsRequiredOnlyWithPlanCreationEvidence(t *testing.T) {
	planDigest := strings.Repeat("d", 64)
	repo := filepath.Join(t.TempDir(), "repo")
	hostGit(t, "", "init", "-b", "main", repo)
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("approved\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	approved, err := onboarding.BaselineSnapshot(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	head, err := onboarding.CreateInitialBaseline(context.Background(), repo, "main", approved, "Initial Repository Baseline\n\nSetup-Plan-SHA256: "+planDigest)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/owner/repo":
			_, _ = w.Write([]byte(`{"full_name":"owner/repo","default_branch":"main","private":true}`))
		case "/repos/owner/repo/git/ref/heads/main":
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"message":"Git Repository is empty."}`))
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()
	filesJSON, _ := json.Marshal(approved)
	effect := setupcontract.Effect{ID: "initial-baseline", Kind: "initial_baseline", Subject: repo, Action: "commit_and_push", Parameters: map[string]string{"branch": "main", "files_json": string(filesJSON), "repository": "owner/repo", "source_url": "https://github.com/owner/repo.git"}}
	for _, test := range []struct {
		name    string
		created bool
		want    setupcontract.EffectStatus
		wantErr bool
	}{
		{name: "no creation evidence", want: setupcontract.EffectFailed, wantErr: true},
		{name: "same plan creation evidence", created: true, want: setupcontract.EffectRequired},
	} {
		t.Run(test.name, func(t *testing.T) {
			adapter := HostAdapter{GitHub: workflowgithub.NewClient(server.URL, "token", server.Client()), PlanDigest: planDigest, CreatedRepositories: map[string]bool{"owner/repo": test.created}, InitialBaselineHeads: map[string]string{}}
			status, _, readErr := adapter.Readback(context.Background(), effect)
			if status != test.want || (readErr != nil) != test.wantErr {
				t.Fatalf("empty remote readback for %s = %s, %v; want %s err=%t (local head %s)", test.name, status, readErr, test.want, test.wantErr, head)
			}
		})
	}
}

func TestCreateRepositoryReadbackRejectsTakeoverWithoutThisPlanEvidence(t *testing.T) {
	for _, test := range []struct {
		name, fullName   string
		private, created bool
		want             setupcontract.EffectStatus
	}{
		{name: "matching takeover", fullName: "owner/repo", private: true, want: setupcontract.EffectConflicting},
		{name: "plan-created exact repository", fullName: "owner/repo", private: true, created: true, want: setupcontract.EffectSatisfied},
		{name: "plan-created visibility drift", fullName: "owner/repo", private: false, created: true, want: setupcontract.EffectConflicting},
		{name: "plan-created owner drift", fullName: "attacker/repo", private: true, created: true, want: setupcontract.EffectConflicting},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"full_name":"` + test.fullName + `","private":` + boolStringForTest(test.private) + `}`))
			}))
			defer server.Close()
			adapter := HostAdapter{GitHub: workflowgithub.NewClient(server.URL, "token", server.Client()), CreatedRepositories: map[string]bool{"owner/repo": test.created}}
			effect := setupcontract.Effect{ID: "create-repository", Kind: "create_repository", Subject: "owner/repo", Action: "create", Parameters: map[string]string{"owner": "owner", "authenticated_login": "owner", "name": "repo", "private": "true", "approval_absent_repository": "owner/repo"}}
			status, _, err := adapter.Readback(context.Background(), effect)
			if err != nil || status != test.want {
				t.Fatalf("create repository readback = %s, %v; want %s", status, err, test.want)
			}
		})
	}
}

func TestCreateRepositoryReadbackAcceptsExactRepositoryAfterRestoredStartedAttempt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"full_name":"owner/repo","private":true}`))
	}))
	defer server.Close()
	digest := strings.Repeat("a", 64)
	adapter := HostAdapter{GitHub: workflowgithub.NewClient(server.URL, "token", server.Client()), PlanDigest: digest, CreatedRepositories: map[string]bool{}}
	effect := setupcontract.Effect{ID: "create-repository", Kind: "create_repository", Subject: "owner/repo", Action: "create", Parameters: map[string]string{"owner": "owner", "authenticated_login": "owner", "name": "repo", "private": "true", "approval_absent_repository": "owner/repo"}}
	events := []store.SetupRepositoryCreateAttemptEvent{{
		PlanID: "onboarding", PlanDigestSHA256: digest, EffectID: effect.ID, ExecutionAttempt: 1, Event: store.RepositoryCreateStarted,
		Owner: "owner", Name: "repo", Private: true, ApprovalAbsentRepository: "owner/repo", RecordedAt: time.Now().UTC(),
	}}
	if err := adapter.RestoreRepositoryCreateAttemptEvents(effect, events); err != nil {
		t.Fatal(err)
	}
	status, _, err := adapter.Readback(context.Background(), effect)
	if err != nil || status != setupcontract.EffectSatisfied {
		t.Fatalf("restored unknown-outcome readback = %s, %v", status, err)
	}
}

func TestCreateRepositoryReadbackRejectsRepositoryAfterDefinitiveFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"full_name":"owner/repo","private":true}`))
	}))
	defer server.Close()
	digest := strings.Repeat("a", 64)
	adapter := HostAdapter{GitHub: workflowgithub.NewClient(server.URL, "token", server.Client()), PlanDigest: digest, CreatedRepositories: map[string]bool{}}
	now := time.Now().UTC()
	events := []store.SetupRepositoryCreateAttemptEvent{
		{PlanID: "onboarding", PlanDigestSHA256: digest, EffectID: "create-repository", ExecutionAttempt: 1, Event: store.RepositoryCreateStarted, Owner: "owner", Name: "repo", Private: true, ApprovalAbsentRepository: "owner/repo", RecordedAt: now},
		{PlanID: "onboarding", PlanDigestSHA256: digest, EffectID: "create-repository", ExecutionAttempt: 1, Event: store.RepositoryCreateDefinitiveFailure, Owner: "owner", Name: "repo", Private: true, ApprovalAbsentRepository: "owner/repo", RecordedAt: now.Add(time.Second)},
	}
	effect := setupcontract.Effect{ID: "create-repository", Kind: "create_repository", Subject: "owner/repo", Action: "create", Parameters: map[string]string{"owner": "owner", "authenticated_login": "owner", "name": "repo", "private": "true", "approval_absent_repository": "owner/repo"}}
	if err := adapter.RestoreRepositoryCreateAttemptEvents(effect, events); err != nil {
		t.Fatal(err)
	}
	status, _, err := adapter.Readback(context.Background(), effect)
	if err != nil || status != setupcontract.EffectConflicting {
		t.Fatalf("definitive-failure readback = %s, %v", status, err)
	}
}

func TestRepositoryCreateOutcomeClassification(t *testing.T) {
	adapter := HostAdapter{}
	if !adapter.RepositoryCreateOutcomeUnknown(context.DeadlineExceeded) {
		t.Fatal("transport timeout was treated as a definitive create failure")
	}
	if adapter.RepositoryCreateOutcomeUnknown(&workflowgithub.APIError{StatusCode: http.StatusUnprocessableEntity}) {
		t.Fatal("GitHub validation rejection was treated as an unknown create outcome")
	}
	if !adapter.RepositoryCreateOutcomeUnknown(&workflowgithub.APIError{StatusCode: http.StatusInternalServerError}) {
		t.Fatal("GitHub server failure was treated as a definitive create outcome")
	}
}

func boolStringForTest(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func TestRepositoryContractZeroCommitBaseComesOnlyFromPersistedBaselineEvidence(t *testing.T) {
	remoteHead := strings.Repeat("e", 40)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/owner/repo":
			_, _ = w.Write([]byte(`{"default_branch":"main"}`))
		case "/repos/owner/repo/git/ref/heads/main":
			_, _ = w.Write([]byte(`{"object":{"sha":"` + remoteHead + `"}}`))
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()
	effect := setupcontract.Effect{ID: "repository-contract-pr", Kind: "repository_contract_pr", Subject: "owner/repo", Action: "create_check_merge", Parameters: map[string]string{
		"base_branch": "main", "base_head": "", "base_head_effect_id": "initial-baseline", "source_url": "https://github.com/owner/repo.git", "before_files_json": `{}`, "files_json": `{}`, "manifest_digest": strings.Repeat("a", 64), "required_checks_json": `[{"context":"workflow-contract","app_id":15368}]`,
	}}
	adapter := HostAdapter{GitHub: workflowgithub.NewClient(server.URL, "token", server.Client()), PlanDigest: strings.Repeat("b", 64), TemporaryRoot: t.TempDir(), OnboardingMergeHeads: map[string]string{}, InitialBaselineHeads: map[string]string{}}
	err := adapter.Apply(context.Background(), effect, &SecretInput{})
	if err == nil || !strings.Contains(err.Error(), "persisted Initial Repository Baseline HEAD") {
		t.Fatalf("runtime remote selected without baseline evidence: %v", err)
	}
	adapter.InitialBaselineHeads["initial-baseline"] = strings.Repeat("d", 40)
	err = adapter.Apply(context.Background(), effect, &SecretInput{})
	if err == nil || !strings.Contains(err.Error(), "base drifted") {
		t.Fatalf("external remote advance accepted over persisted baseline: %v", err)
	}
}

func TestHostAdapterRestoresPlanBoundPublicationEvidence(t *testing.T) {
	adapter := HostAdapter{CreatedRepositories: map[string]bool{}, PublishedHistoryHeads: map[string]string{}, InitialBaselineHeads: map[string]string{}, ApprovedGitHubPolicies: map[string]string{}}
	published := strings.Repeat("a", 40)
	baseline := strings.Repeat("b", 40)
	policy := approvedPolicyJSON(t, onboarding.RepositoryPolicy{ActionsEnabled: true, ActionsAllowed: "all", GitHubOwnedActionsAllowed: true, AllowSquashMerge: true})
	err := adapter.RestoreEffectResults([]setupcontract.EffectResult{
		{EffectID: "create", Status: setupcontract.EffectSatisfied, Evidence: "created; " + repositoryCreatedPolicyEvidence + base64.StdEncoding.EncodeToString([]byte(policy)) + "; " + repositoryCreatedEvidence + "create:owner/repo"},
		{EffectID: "publish", Evidence: "published; " + publishedHistoryHeadEvidence + published},
		{EffectID: "baseline", Evidence: "baseline; " + initialBaselineHeadEvidence + baseline},
	})
	if err != nil || !adapter.CreatedRepositories["owner/repo"] || adapter.PublishedHistoryHeads["publish"] != published || adapter.InitialBaselineHeads["baseline"] != baseline || adapter.ApprovedGitHubPolicies["owner/repo"] != policy {
		t.Fatalf("restored publication evidence = %#v %#v %#v %#v err=%v", adapter.CreatedRepositories, adapter.PublishedHistoryHeads, adapter.InitialBaselineHeads, adapter.ApprovedGitHubPolicies, err)
	}
}

func TestHostAdapterRejectsRepositoryCreationMarkerNotBoundToSatisfiedEffect(t *testing.T) {
	for _, result := range []setupcontract.EffectResult{
		{EffectID: "create", Status: setupcontract.EffectFailed, Evidence: repositoryCreatedEvidence + "create:owner/repo"},
		{EffectID: "other", Status: setupcontract.EffectSatisfied, Evidence: repositoryCreatedEvidence + "create:owner/repo"},
		{EffectID: "create", Status: setupcontract.EffectSatisfied, Evidence: repositoryCreatedEvidence + "owner/repo"},
	} {
		adapter := HostAdapter{CreatedRepositories: map[string]bool{}}
		if err := adapter.RestoreEffectResults([]setupcontract.EffectResult{result}); err == nil || adapter.CreatedRepositories["owner/repo"] {
			t.Fatalf("unbound repository creation evidence restored: %#v err=%v", result, err)
		}
	}
}

func TestRepositoryContractRechecksDefaultHeadImmediatelyBeforePRCreation(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	hostGit(t, "", "init", "-b", "main", source)
	hostGit(t, source, "config", "user.name", "Test")
	hostGit(t, source, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hostGit(t, source, "add", "README.md")
	hostGit(t, source, "commit", "-m", "base")
	base := hostGitOutput(t, source, "rev-parse", "HEAD")
	bare := filepath.Join(t.TempDir(), "remote.git")
	hostGit(t, "", "clone", "--bare", source, bare)

	refReads := 0
	created := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo":
			_, _ = w.Write([]byte(`{"default_branch":"main"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/git/ref/heads/main":
			refReads++
			head := base
			if refReads > 1 {
				head = strings.Repeat("b", 40)
			}
			_, _ = w.Write([]byte(`{"object":{"sha":"` + head + `"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls":
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && r.URL.Path == "/repos/owner/repo/pulls":
			created = true
			_, _ = w.Write([]byte(`{"number":7}`))
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/repos/owner/repo/git/refs/heads/workflow/onboarding-"):
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()
	files, _ := json.Marshal(map[string]string{"managed.txt": base64.StdEncoding.EncodeToString([]byte("contract\n"))})
	effect := setupcontract.Effect{Kind: "repository_contract_pr", Subject: "owner/repo", Parameters: map[string]string{
		"files_json": string(files), "source_url": bare, "base_head": base, "base_branch": "main", "required_checks_json": `[{"context":"workflow-contract","app_id":15368}]`,
	}}
	adapter := HostAdapter{GitHub: workflowgithub.NewClient(server.URL, "token", server.Client()).WithRepositoryOwner("owner"), PlanDigest: strings.Repeat("a", 64), TemporaryRoot: t.TempDir(), OnboardingMergeHeads: map[string]string{}}
	err := adapter.applyRepositoryContract(context.Background(), effect)
	if err == nil || !strings.Contains(err.Error(), "base drifted") {
		t.Fatalf("default-head drift accepted: %v", err)
	}
	if created {
		t.Fatal("pull request was created after the approved base drifted")
	}
}

func TestRepositoryContractReadbackBindsMergedDefaultHeadAndRejectsLaterCommit(t *testing.T) {
	digest := strings.Repeat("d", 64)
	mergedHead := strings.Repeat("a", 40)
	defaultHead := mergedHead
	pullBase := strings.Repeat("c", 40)
	contractFiles, _, manifestDigest, err := repositorycontract.Render("single-context", nil, "owner/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/owner/repo/contents/"):
			path := strings.TrimPrefix(r.URL.Path, "/repos/owner/repo/contents/")
			content, ok := contractFiles[path]
			if !ok {
				t.Fatalf("unexpected repository file %q", path)
			}
			_, _ = w.Write([]byte(`{"encoding":"base64","content":"` + base64.StdEncoding.EncodeToString(content) + `"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls":
			if r.URL.Query().Get("state") != "all" {
				t.Fatalf("pull request readback state = %q", r.URL.Query().Get("state"))
			}
			_, _ = w.Write([]byte(`[{"number":7,"body":"Approved Setup Plan SHA-256: ` + digest + `","merged_at":"2026-08-15T00:00:00Z","merge_commit_sha":"` + mergedHead + `","head":{"sha":"` + strings.Repeat("b", 40) + `","ref":"workflow/onboarding-` + digest[:12] + `"},"base":{"sha":"` + pullBase + `","ref":"main"}}]`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo":
			_, _ = w.Write([]byte(`{"default_branch":"main"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/git/ref/heads/main":
			_, _ = w.Write([]byte(`{"object":{"sha":"` + defaultHead + `"}}`))
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()
	adapter := HostAdapter{GitHub: workflowgithub.NewClient(server.URL, "token", server.Client()).WithRepositoryOwner("owner"), PlanDigest: digest, OnboardingMergeHeads: map[string]string{}}
	effect := setupcontract.Effect{ID: "contract", Kind: "repository_contract_pr", Subject: "owner/repo", Action: "create_check_merge", Parameters: map[string]string{"base_branch": "main", "base_head": strings.Repeat("c", 40), "source_url": "https://github.com/owner/repo.git", "before_files_json": "[]", "files_json": "[]", "manifest_digest": manifestDigest, "required_checks_json": `[{"context":"workflow-contract","app_id":15368}]`}}
	status, _, err := adapter.Readback(context.Background(), effect)
	if err != nil || status != setupcontract.EffectSatisfied {
		t.Fatalf("merged readback = %s, %v", status, err)
	}
	if adapter.OnboardingMergeHeads[effect.ID] != mergedHead {
		t.Fatalf("durable merge binding = %#v", adapter.OnboardingMergeHeads)
	}
	pullBase = strings.Repeat("f", 40)
	status, evidence, err := adapter.Readback(context.Background(), effect)
	if err != nil || status != setupcontract.EffectConflicting || !strings.Contains(evidence, "base") {
		t.Fatalf("pull request on unapproved base accepted = %s, %q, %v", status, evidence, err)
	}
	pullBase = strings.Repeat("c", 40)
	restored := HostAdapter{OnboardingMergeHeads: map[string]string{}}
	if err := restored.RestoreEffectResults([]setupcontract.EffectResult{{EffectID: effect.ID, Evidence: onboardingMergeHeadEvidence + mergedHead}}); err != nil || restored.OnboardingMergeHeads[effect.ID] != mergedHead {
		t.Fatalf("restored merge binding = %#v, %v", restored.OnboardingMergeHeads, err)
	}
	defaultHead = strings.Repeat("e", 40)
	status, evidence, err = adapter.Readback(context.Background(), effect)
	if err != nil || status != setupcontract.EffectConflicting || !strings.Contains(evidence, "advanced") {
		t.Fatalf("post-merge extra commit readback = %s, %q, %v", status, evidence, err)
	}
}

func TestRepositoryContractReadbackDistinguishesExactOpenClosedAndExternalConflict(t *testing.T) {
	digest := strings.Repeat("d", 64)
	base := strings.Repeat("c", 40)
	branch := "workflow/onboarding-" + digest[:12]
	effect := setupcontract.Effect{ID: "contract", Kind: "repository_contract_pr", Subject: "owner/repo", Action: "create_check_merge", Parameters: map[string]string{
		"base_branch": "main", "base_head": base, "source_url": "https://github.com/owner/repo.git", "before_files_json": "[]", "files_json": "[]", "manifest_digest": strings.Repeat("a", 64), "required_checks_json": `[{"context":"workflow-contract","app_id":15368}]`,
	}}
	for _, test := range []struct {
		name       string
		state      string
		bodyDigest string
		wantStatus setupcontract.EffectStatus
		wantText   string
	}{
		{name: "open same plan", state: "open", bodyDigest: digest, wantStatus: setupcontract.EffectRequired, wantText: "open"},
		{name: "closed same plan", state: "closed", bodyDigest: digest, wantStatus: setupcontract.EffectRequired, wantText: "replacement"},
		{name: "closed external conflict", state: "closed", bodyDigest: strings.Repeat("e", 64), wantStatus: setupcontract.EffectConflicting, wantText: "differs"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/repos/owner/repo/pulls" {
					t.Fatalf("readback continued past pull request classification to %s %s", r.Method, r.URL.String())
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`[{"number":7,"state":"` + test.state + `","body":"Approved Setup Plan SHA-256: ` + test.bodyDigest + `","head":{"sha":"` + strings.Repeat("b", 40) + `","ref":"` + branch + `"},"base":{"sha":"` + base + `","ref":"main"}}]`))
			}))
			defer server.Close()
			adapter := HostAdapter{GitHub: workflowgithub.NewClient(server.URL, "token", server.Client()), PlanDigest: digest, OnboardingMergeHeads: map[string]string{}}
			status, evidence, err := adapter.Readback(context.Background(), effect)
			if err != nil || status != test.wantStatus || !strings.Contains(strings.ToLower(evidence), test.wantText) {
				t.Fatalf("readback = %s, %q, %v; want %s containing %q", status, evidence, err, test.wantStatus, test.wantText)
			}
		})
	}
}

func TestRepositoryContractClosedSamePlanAttemptIsDeletedAndReplaced(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	hostGit(t, "", "init", "-b", "main", source)
	hostGit(t, source, "config", "user.name", "Test")
	hostGit(t, source, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hostGit(t, source, "add", "README.md")
	hostGit(t, source, "commit", "-m", "base")
	base := hostGitOutput(t, source, "rev-parse", "HEAD")
	bare := filepath.Join(t.TempDir(), "remote.git")
	hostGit(t, "", "clone", "--bare", source, bare)
	digest := strings.Repeat("a", 64)
	branch := "workflow/onboarding-" + digest[:12]
	contractFiles := map[string][]byte{"managed.txt": []byte("contract\n")}
	preview, err := onboarding.PrepareOnboardingCommit(context.Background(), "owner/repo", bare, base, t.TempDir(), digest, contractFiles, onboarding.GitCredential{})
	if err != nil {
		t.Fatal(err)
	}
	approvedHead := preview.Head
	if err := preview.Cleanup(); err != nil {
		t.Fatal(err)
	}
	mergeHead := strings.Repeat("c", 40)
	deleted, created, merged := false, false, false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		workspaceHead := func() string { return hostGitOutput(t, "", "--git-dir", bare, "rev-parse", "refs/heads/"+branch) }
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls":
			_, _ = w.Write([]byte(`[{"number":6,"state":"closed","body":"Approved Setup Plan SHA-256: ` + digest + `","head":{"sha":"` + approvedHead + `","ref":"` + branch + `"},"base":{"sha":"` + base + `","ref":"main"}}]`))
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/git/refs/heads/"):
			deleted = true
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/owner/repo/pulls":
			if !deleted {
				t.Fatal("replacement PR created before closed branch cleanup")
			}
			created = true
			_, _ = w.Write([]byte(`{"number":7,"state":"open"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo":
			_, _ = w.Write([]byte(`{"default_branch":"main","allow_squash_merge":true,"has_issues":true}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/git/ref/heads/main":
			head := base
			if merged {
				head = mergeHead
			}
			_, _ = w.Write([]byte(`{"object":{"sha":"` + head + `"}}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/check-runs"):
			_, _ = w.Write([]byte(`{"check_runs":[{"name":"workflow-contract","status":"completed","conclusion":"success","head_sha":"` + workspaceHead() + `","app":{"id":15368}}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls/7":
			_, _ = w.Write([]byte(`{"number":7,"state":"open","mergeable":true,"head":{"sha":"` + workspaceHead() + `","ref":"` + branch + `"},"base":{"sha":"` + base + `","ref":"main"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls/7/reviews":
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/actions/permissions":
			_, _ = w.Write([]byte(`{"enabled":true,"allowed_actions":"all"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/branches/main/protection":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"not found"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/rulesets":
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPut && r.URL.Path == "/repos/owner/repo/pulls/7/merge":
			merged = true
			_, _ = w.Write([]byte(`{"merged":true,"sha":"` + mergeHead + `"}`))
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()
	files, _ := json.Marshal(map[string]string{"managed.txt": base64.StdEncoding.EncodeToString(contractFiles["managed.txt"])})
	adapter := HostAdapter{
		GitHub: workflowgithub.NewClient(server.URL, "token", server.Client()).WithOnboardingIdentity("owner", "owner", "owner/repo"), PlanDigest: digest, TemporaryRoot: t.TempDir(), OnboardingMergeHeads: map[string]string{},
		ApprovedGitHubPolicies: map[string]string{"owner/repo": approvedPolicyJSON(t, onboarding.RepositoryPolicy{HasIssues: true, ActionsEnabled: true, ActionsAllowed: "all", GitHubOwnedActionsAllowed: true, AllowSquashMerge: true, AllowFeatureEnable: true})},
	}
	effect := setupcontract.Effect{ID: "repository-contract-pr", Kind: "repository_contract_pr", Subject: "owner/repo", Action: "create_check_merge", Parameters: map[string]string{"files_json": string(files), "source_url": bare, "base_head": base, "base_branch": "main", "required_checks_json": `[{"context":"workflow-contract","app_id":15368}]`}}
	if err := adapter.applyRepositoryContract(context.Background(), effect); err != nil {
		t.Fatal(err)
	}
	if !deleted || !created || !merged {
		t.Fatalf("replacement lifecycle delete=%t create=%t merge=%t", deleted, created, merged)
	}
}

func TestRepositoryContractClosedPlanHeadDriftDoesNotDeleteOrReplace(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	hostGit(t, "", "init", "-b", "main", source)
	hostGit(t, source, "config", "user.name", "Test")
	hostGit(t, source, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hostGit(t, source, "add", "README.md")
	hostGit(t, source, "commit", "-m", "base")
	base := hostGitOutput(t, source, "rev-parse", "HEAD")
	bare := filepath.Join(t.TempDir(), "remote.git")
	hostGit(t, "", "clone", "--bare", source, bare)
	digest := strings.Repeat("a", 64)
	branch := "workflow/onboarding-" + digest[:12]
	deleted, created := false, false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls":
			_, _ = w.Write([]byte(`[{"number":6,"state":"closed","body":"Approved Setup Plan SHA-256: ` + digest + `","head":{"sha":"` + strings.Repeat("b", 40) + `","ref":"` + branch + `"},"base":{"sha":"` + base + `","ref":"main"}}]`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo":
			_, _ = w.Write([]byte(`{"default_branch":"main"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/git/ref/heads/main":
			_, _ = w.Write([]byte(`{"object":{"sha":"` + base + `"}}`))
		case r.Method == http.MethodDelete:
			deleted = true
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost:
			created = true
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	files, _ := json.Marshal(map[string]string{"managed.txt": base64.StdEncoding.EncodeToString([]byte("contract\n"))})
	adapter := HostAdapter{GitHub: workflowgithub.NewClient(server.URL, "token", server.Client()).WithOnboardingIdentity("owner", "owner", "owner/repo"), PlanDigest: digest, TemporaryRoot: t.TempDir(), OnboardingMergeHeads: map[string]string{}}
	effect := setupcontract.Effect{ID: "repository-contract-pr", Kind: "repository_contract_pr", Subject: "owner/repo", Action: "create_check_merge", Parameters: map[string]string{"files_json": string(files), "source_url": bare, "base_head": base, "base_branch": "main", "required_checks_json": `[{"context":"workflow-contract","app_id":15368}]`}}
	if err := adapter.applyRepositoryContract(context.Background(), effect); err == nil {
		t.Fatal("closed pull request with an unapproved head was accepted")
	}
	if deleted || created {
		t.Fatalf("closed pull request head drift reached mutation: deleted=%t created=%t", deleted, created)
	}
	command := exec.Command("git", "--git-dir", bare, "rev-parse", "--verify", "refs/heads/"+branch)
	if err := command.Run(); err == nil {
		t.Fatal("closed pull request head drift published an approved replacement branch before conflict")
	}
}

func TestDefaultHeadPreconditionRejectsMatchingManifestWithoutPersistedMergeEvidence(t *testing.T) {
	manifest := []byte(`{"schema_version":1}`)
	manifestDigest := sha256.Sum256(manifest)
	driftedHead := strings.Repeat("b", 40)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/owner/repo":
			_, _ = w.Write([]byte(`{"default_branch":"main"}`))
		case "/repos/owner/repo/git/ref/heads/main":
			_, _ = w.Write([]byte(`{"object":{"sha":"` + driftedHead + `"}}`))
		case "/repos/owner/repo/contents/.workflow/repository.json":
			_, _ = w.Write([]byte(`{"encoding":"base64","content":"` + base64.StdEncoding.EncodeToString(manifest) + `"}`))
		default:
			t.Fatalf("unexpected request %s", r.URL.String())
		}
	}))
	defer server.Close()
	expected, _ := json.Marshal(map[string]string{"branch": "main", "head": strings.Repeat("a", 40), "manifest_digest": hex.EncodeToString(manifestDigest[:])})
	adapter := HostAdapter{GitHub: workflowgithub.NewClient(server.URL, "token", server.Client()).WithRepositoryOwner("owner"), OnboardingMergeHeads: map[string]string{}}
	err := adapter.CheckPrecondition(context.Background(), setupcontract.Precondition{ID: "head", Kind: "github_default_head", Subject: "owner/repo", Expected: string(expected)})
	if err == nil || !strings.Contains(err.Error(), "drifted") {
		t.Fatalf("matching manifest accepted arbitrary remote HEAD: %v", err)
	}
	adapter.OnboardingMergeHeads["repository-contract-pr"] = driftedHead
	if err := adapter.CheckPrecondition(context.Background(), setupcontract.Precondition{ID: "head", Kind: "github_default_head", Subject: "owner/repo", Expected: string(expected)}); err != nil {
		t.Fatalf("exact persisted same-plan merge HEAD was not accepted: %v", err)
	}
}

func TestLocalFastForwardRejectsRemoteAdvancePastPersistedMergeHead(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	hostGit(t, "", "init", "-b", "main", repo)
	hostGit(t, repo, "config", "user.name", "Test")
	hostGit(t, repo, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hostGit(t, repo, "add", "README.md")
	hostGit(t, repo, "commit", "-m", "base")
	preMerge := hostGitOutput(t, repo, "rev-parse", "HEAD")
	mergeHead := strings.Repeat("a", 40)
	extraHead := strings.Repeat("b", 40)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/owner/repo":
			_, _ = w.Write([]byte(`{"default_branch":"main"}`))
		case "/repos/owner/repo/git/ref/heads/main":
			_, _ = w.Write([]byte(`{"object":{"sha":"` + extraHead + `"}}`))
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()
	adapter := HostAdapter{GitHub: workflowgithub.NewClient(server.URL, "token", server.Client()), OnboardingMergeHeads: map[string]string{"repository-contract-pr": mergeHead}}
	effect := setupcontract.Effect{ID: "synchronize", Kind: "local_fast_forward", Subject: repo, Action: "fast_forward_if_safe", Parameters: map[string]string{"repository": "owner/repo", "branch": "main", "pre_merge_head": preMerge, "merge_head_effect_id": "repository-contract-pr"}}
	status, evidence, err := adapter.Readback(context.Background(), effect)
	if err != nil || status != setupcontract.EffectConflicting || !strings.Contains(evidence, "advanced") {
		t.Fatalf("remote post-merge advance readback = %s, %q, %v", status, evidence, err)
	}
}

func hostGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

func hostGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(output))
}

func approvedPolicyJSON(t *testing.T, policy onboarding.RepositoryPolicy) string {
	t.Helper()
	raw, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func patFingerprint(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return hex.EncodeToString(sum[:])
}

func TestHostAdapterReconcilesPersistedProbeCleanupResources(t *testing.T) {
	root := t.TempDir()
	stateRoot, workspaceRoot := filepath.Join(root, "state"), filepath.Join(root, "workspaces")
	dockerProbe := filepath.Join(stateRoot, ".setup-readiness-abcdef123456")
	codexProbe := filepath.Join(os.TempDir(), "workflow-doctor-codex-abcdef123456")
	var paths, containers []string
	resource := func(value string) string {
		raw, _ := json.Marshal(map[string]string{"value": value})
		return string(raw)
	}
	adapter := HostAdapter{
		Layout:                 workflowhome.Layout{State: stateRoot, Workspaces: workspaceRoot},
		RemoveCleanupPath:      func(path string) error { paths = append(paths, path); return nil },
		RemoveCleanupContainer: func(_ context.Context, name string) error { containers = append(containers, name); return nil },
	}
	for _, obligation := range []store.SetupCleanupObligation{
		{Kind: "docker_state_probe", Resource: resource(dockerProbe)},
		{Kind: "docker_workspace_probe", Resource: resource(filepath.Join(workspaceRoot, ".setup-readiness-abcdef123456"))},
		{Kind: "codex_temp_dir", Resource: resource(codexProbe)},
		{Kind: "docker_container", Resource: resource("workflow-setup-docker-abcdef123456")},
		{Kind: "codex_container", Resource: resource("workflow-doctor-codex-abcdef123456-initial")},
	} {
		if err := adapter.ReconcileCleanupObligation(context.Background(), obligation); err != nil {
			t.Fatal(err)
		}
	}
	if len(paths) != 3 || len(containers) != 2 {
		t.Fatalf("paths=%#v containers=%#v", paths, containers)
	}
	bad := store.SetupCleanupObligation{Kind: "docker_container", Resource: resource("unowned")}
	if err := adapter.ReconcileCleanupObligation(context.Background(), bad); err == nil {
		t.Fatal("unowned container cleanup accepted")
	}
}

func TestGlobalCleanupDrainRejectsDatabaseInjectedTargetsBeforeDeletion(t *testing.T) {
	cases := []struct {
		name, kind, effectID, obligationID, resource string
	}{
		{"branch", "remote_onboarding_branch", "first", "first:remote-branch", `{"repository":"owner/repo","branch":"workflow/onboarding-aaaaaaaaaaaa"}`},
		{"temporary clone", "temporary_clone", "first", "first:temporary-clone", `{"root":"C:/","path":"C:/workflow-onboarding-aaaaaaaaaaaa"}`},
		{"Docker", "docker_container", "ready", "ready:docker-container", `{"value":"workflow-setup-docker-aaaaaaaaaaaa"}`},
		{"Codex", "codex_temp_dir", "ready", "ready:codex-temp-dir", `{"value":"C:/Windows/workflow-doctor-codex-aaaaaaaaaaaa"}`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			layout, err := workflowhome.Resolve(filepath.Join(t.TempDir(), "home"))
			if err != nil {
				t.Fatal(err)
			}
			if err := layout.Ensure(); err != nil {
				t.Fatal(err)
			}
			plan := testPlan(layout.Root)
			raw, _ := json.Marshal(plan)
			_, canonical, digest, err := setupcontract.ParsePlan(raw)
			if err != nil {
				t.Fatal(err)
			}
			db, err := store.Open(context.Background(), filepath.Join(layout.State, "workflow.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := db.RecordSetupPlan(context.Background(), store.SetupPlanRecord{PlanID: plan.PlanID, Kind: string(plan.Kind), SchemaVersion: plan.SchemaVersion, Target: layout.Root, DigestSHA256: digest, CanonicalJSON: string(canonical), Projection: Project(plan, digest), CreatedAt: testTime()}); err != nil {
				t.Fatal(err)
			}
			obligation := store.SetupCleanupObligation{PlanID: plan.PlanID, PlanDigestSHA256: digest, EffectID: test.effectID, ObligationID: test.obligationID, Kind: test.kind, Resource: test.resource, Status: store.CleanupPending, UpdatedAt: testTime()}
			if err := db.RecordSetupCleanupObligation(context.Background(), obligation); err != nil {
				t.Fatal(err)
			}
			deleted := 0
			adapter := HostAdapter{Layout: layout, RemoveCleanupPath: func(string) error { deleted++; return nil }, RemoveCleanupContainer: func(context.Context, string) error { deleted++; return nil }}
			if err := DrainPendingCleanupObligations(context.Background(), db, adapter, testTime()); err == nil || deleted != 0 {
				t.Fatalf("err=%v deleted=%d", err, deleted)
			}
			pending, err := db.PendingSetupCleanupObligationsAll(context.Background())
			if err != nil || len(pending) != 1 {
				t.Fatalf("pending=%#v err=%v", pending, err)
			}
		})
	}
}

func TestHostAdapterAcceptsOnlyExactDeterministicCleanupTargets(t *testing.T) {
	layout, err := workflowhome.Resolve(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	adapter := HostAdapter{Layout: layout}
	platformPlan := testPlan(layout.Root)
	platformRaw, _ := json.Marshal(platformPlan)
	_, _, platformDigest, err := setupcontract.ParsePlan(platformRaw)
	if err != nil {
		t.Fatal(err)
	}
	value := func(item string) string { raw, _ := json.Marshal(map[string]string{"value": item}); return string(raw) }
	for _, obligation := range []store.SetupCleanupObligation{
		{PlanID: platformPlan.PlanID, PlanDigestSHA256: platformDigest, EffectID: "ready", ObligationID: "ready:docker-state-probe", Kind: "docker_state_probe", Resource: value(filepath.Join(layout.State, ".setup-readiness-"+platformDigest[:12]))},
		{PlanID: platformPlan.PlanID, PlanDigestSHA256: platformDigest, EffectID: "ready", ObligationID: "ready:docker-workspace-probe", Kind: "docker_workspace_probe", Resource: value(filepath.Join(layout.Workspaces, ".setup-readiness-"+platformDigest[:12]))},
		{PlanID: platformPlan.PlanID, PlanDigestSHA256: platformDigest, EffectID: "ready", ObligationID: "ready:docker-container", Kind: "docker_container", Resource: value("workflow-setup-docker-" + platformDigest[:12])},
		{PlanID: platformPlan.PlanID, PlanDigestSHA256: platformDigest, EffectID: "ready", ObligationID: "ready:codex-temp-dir", Kind: "codex_temp_dir", Resource: value(filepath.Join(os.TempDir(), "workflow-doctor-codex-"+platformDigest[:12]))},
		{PlanID: platformPlan.PlanID, PlanDigestSHA256: platformDigest, EffectID: "ready", ObligationID: "ready:codex-container-initial", Kind: "codex_container", Resource: value("workflow-doctor-codex-" + platformDigest[:12] + "-initial")},
	} {
		if err := adapter.ValidateCleanupObligation(platformPlan, obligation); err != nil {
			t.Fatalf("valid obligation %#v: %v", obligation, err)
		}
	}
	repoPlan := setupcontract.Plan{
		SchemaVersion: 1, PlanID: "repository-cleanup", Kind: setupcontract.RepositoryOnboarding,
		Target:        setupcontract.Target{WorkflowHome: layout.Root, RepositoryPath: `C:\repo`, GitHubRepository: "owner/repo"},
		Preconditions: []setupcontract.Precondition{{ID: "head", Kind: "git_head", Subject: `C:\repo`, Expected: strings.Repeat("a", 40)}},
		Effects: []setupcontract.Effect{
			{ID: "contract", Kind: "repository_contract_pr", Subject: "owner/repo", Action: "create_check_merge", Parameters: map[string]string{"base_branch": "main", "base_head": strings.Repeat("a", 40), "source_url": "https://github.com/owner/repo.git", "before_files_json": `{}`, "files_json": `{}`, "manifest_digest": strings.Repeat("b", 64), "required_checks_json": `[{"context":"workflow-contract","app_id":15368}]`}},
			{ID: "admit", Kind: "repository_admission", Subject: "owner/repo", Action: "verify_and_record", Parameters: map[string]string{"default_branch": "main", "manifest_digest": strings.Repeat("b", 64), "contract_version": "1"}},
		},
		ExpectedResults: []setupcontract.ExpectedResult{{ID: "ready", Kind: "repository_admission", Subject: "owner/repo", Expected: strings.Repeat("b", 64)}},
	}
	repoRaw, _ := json.Marshal(repoPlan)
	_, _, repoDigest, err := setupcontract.ParsePlan(repoRaw)
	if err != nil {
		t.Fatal(err)
	}
	repositoryObligations, err := adapter.CleanupObligations(repoPlan.Effects[0], repoDigest)
	if err != nil {
		t.Fatal(err)
	}
	for _, obligation := range repositoryObligations {
		obligation.PlanID, obligation.PlanDigestSHA256, obligation.EffectID = repoPlan.PlanID, repoDigest, "contract"
		if err := adapter.ValidateCleanupObligation(repoPlan, obligation); err != nil {
			t.Fatalf("valid repository obligation %#v: %v", obligation, err)
		}
	}
}

func TestHostAdapterPersistsCurrentUserPathAfterInstallingCLI(t *testing.T) {
	layout, err := workflowhome.Resolve(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "workflow.exe")
	if err := os.WriteFile(source, []byte("workflow executable"), 0o700); err != nil {
		t.Fatal(err)
	}
	var persisted string
	adapter := HostAdapter{Layout: layout, Executable: source, PersistUserPATH: func(path string) error {
		persisted = path
		return nil
	}}
	digest := sha256.Sum256([]byte("workflow executable"))
	effect := setupcontract.Effect{ID: "install", Kind: "platform_cli", Subject: filepath.Join(layout.Bin, workflowhome.ExecutableName), Action: "install", Parameters: map[string]string{"version": "1.0.0", "sha256": hex.EncodeToString(digest[:]), "release_manifest_digest": repeat("a", 64), "platform_setup_contract_digest": repeat("b", 64), "workflow_cli_sha256": hex.EncodeToString(digest[:]), "release_bundled_files_digest": repeat("e", 64)}}
	if err := adapter.Apply(context.Background(), effect, &SecretInput{}); err != nil {
		t.Fatal(err)
	}
	if persisted != layout.Bin {
		t.Fatalf("persisted PATH entry = %q, want %q", persisted, layout.Bin)
	}
}

func TestHostAdapterReadbackRequiresExactOwnedCLIVersionAndChecksum(t *testing.T) {
	layout, err := workflowhome.Resolve(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "workflow.exe")
	contents := []byte("workflow executable")
	if err := os.WriteFile(source, contents, 0o700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(contents)
	adapter := HostAdapter{Layout: layout, Executable: source, PersistUserPATH: func(string) error { return nil }, CurrentUserPATHReconciled: func(string) (bool, error) { return true, nil }}
	effect := setupcontract.Effect{ID: "install", Kind: "platform_cli", Subject: filepath.Join(layout.Bin, workflowhome.ExecutableName), Action: "install", Parameters: map[string]string{"version": "1.0.0", "sha256": hex.EncodeToString(sum[:]), "release_manifest_digest": repeat("a", 64), "platform_setup_contract_digest": repeat("b", 64), "workflow_cli_sha256": hex.EncodeToString(sum[:]), "release_bundled_files_digest": repeat("e", 64)}}
	if err := adapter.Apply(context.Background(), effect, nil); err != nil {
		t.Fatal(err)
	}
	status, _, err := adapter.Readback(context.Background(), effect)
	if err != nil || status != setupcontract.EffectSatisfied {
		t.Fatalf("exact readback=%s %v", status, err)
	}
	effect.Parameters["version"] = "1.0.1"
	status, _, err = adapter.Readback(context.Background(), effect)
	if err != nil || status != setupcontract.EffectRequired {
		t.Fatalf("wrong version readback=%s %v", status, err)
	}
	effect.Parameters["version"] = "1.0.0"
	effect.Parameters["sha256"] = strings.Repeat("0", 64)
	status, _, err = adapter.Readback(context.Background(), effect)
	if err != nil || status != setupcontract.EffectRequired {
		t.Fatalf("wrong checksum readback=%s %v", status, err)
	}
}

func TestHostAdapterDockerReadbackRequiresExactDesktopAndLinuxAMD64Engine(t *testing.T) {
	layout, err := workflowhome.Resolve(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	host := &setupDockerHost{version: "4.44.0", engineErr: errors.New("Docker engine is windows/amd64")}
	adapter := HostAdapter{Layout: layout, DockerDesktopHost: host}
	effect := setupcontract.Effect{ID: "docker", Kind: "docker_desktop", Subject: "current-host", Action: "repair", Parameters: map[string]string{"version": "4.45.0", "installer_url": "https://example.test/docker.exe", "windows_amd64_sha256": repeat("a", 64), "release_manifest_digest": repeat("b", 64), "platform_setup_contract_digest": repeat("c", 64), "workflow_cli_sha256": repeat("d", 64), "release_bundled_files_digest": repeat("e", 64)}}
	status, _, err := adapter.Readback(context.Background(), effect)
	if err != nil || status != setupcontract.EffectRequired {
		t.Fatalf("wrong version readback=%s %v", status, err)
	}
	host.version = "4.45.0"
	status, _, err = adapter.Readback(context.Background(), effect)
	if err != nil || status != setupcontract.EffectRequired {
		t.Fatalf("wrong engine readback=%s %v", status, err)
	}
	host.engineErr = nil
	status, _, err = adapter.Readback(context.Background(), effect)
	if err != nil || status != setupcontract.EffectSatisfied {
		t.Fatalf("compatible Docker readback=%s %v", status, err)
	}
}

type setupDockerHost struct {
	version   string
	engineErr error
}

func (h *setupDockerHost) InstalledVersion(context.Context) (string, error) { return h.version, nil }
func (*setupDockerHost) Download(context.Context, string, string) error     { return nil }
func (*setupDockerHost) InstallElevated(context.Context, string) error      { return nil }
func (*setupDockerHost) Start(context.Context) error                        { return nil }
func (h *setupDockerHost) EngineReady(context.Context) error                { return h.engineErr }

func TestHostAdapterStartsAndReadsBackDigestBoundControlPlane(t *testing.T) {
	layout, err := workflowhome.Resolve(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("a", 64)
	record := controlplane.RuntimeRecord{PID: 42, PlatformVersion: "1.0.0", ProcessStartedAt: time.Now().UTC().Round(0), Endpoints: controlplane.Endpoints{Health: "http://127.0.0.1:1234/health", Shutdown: "http://127.0.0.1:1234/shutdown"}, ApprovedPlanDigestSHA256: digest}
	started := false
	adapter := HostAdapter{
		Layout: layout, PlanDigest: digest,
		StartControlPlane: func(_ context.Context, options controlplane.StartOptions) (controlplane.RuntimeRecord, error) {
			started = options.PlatformVersion == "1.0.0" && options.ApprovedPlanDigestSHA256 == digest
			return record, controlplane.WriteRuntimeRecord(layout, record)
		},
		InspectControlPlane: func(context.Context, *controlplane.RuntimeRecord) controlplane.Observation {
			return controlplane.Observation{State: controlplane.StateReady, Record: &record}
		},
	}
	effect := setupcontract.Effect{ID: "serve", Kind: "control_plane", Subject: layout.Root, Action: "start", Parameters: map[string]string{"version": "1.0.0", "release_manifest_digest": repeat("a", 64), "platform_setup_contract_digest": repeat("b", 64), "workflow_cli_sha256": repeat("c", 64), "release_bundled_files_digest": repeat("d", 64)}}
	status, _, err := adapter.Readback(context.Background(), effect)
	if err != nil || status != setupcontract.EffectRequired {
		t.Fatalf("initial readback = %s, %v", status, err)
	}
	if err := adapter.Apply(context.Background(), effect, &SecretInput{}); err != nil {
		t.Fatal(err)
	}
	status, _, err = adapter.Readback(context.Background(), effect)
	if err != nil || status != setupcontract.EffectSatisfied || !started {
		t.Fatalf("final readback = %s, %v, started=%v", status, err, started)
	}
}

func TestHostAdapterInstallsExactWorkflowSkillBundle(t *testing.T) {
	layout, err := workflowhome.Resolve(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "skills")
	if err := os.MkdirAll(filepath.Join(source, "agent-workflow"), 0o700); err != nil {
		t.Fatal(err)
	}
	contents := []byte("# Agent Workflow")
	if err := os.WriteFile(filepath.Join(source, "agent-workflow", "SKILL.md"), contents, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	files, _ := json.Marshal([]workflowhome.SkillBundleFile{{Path: "agent-workflow/SKILL.md", SHA256: hex.EncodeToString(digest[:])}})
	skills, _ := json.Marshal([]string{"agent-workflow"})
	effect := setupcontract.Effect{
		ID: "skills", Kind: "workflow_skill_bundle", Subject: filepath.Join(t.TempDir(), "codex-skills"), Action: "install",
		Parameters: map[string]string{"version": "1.0.0", "managed_skills_json": string(skills), "files_json": string(files), "release_manifest_digest": repeat("a", 64), "platform_setup_contract_digest": repeat("b", 64), "workflow_cli_sha256": repeat("c", 64), "release_bundled_files_digest": repeat("d", 64)},
	}
	adapter := HostAdapter{Layout: layout, SkillBundleSource: source}
	status, _, err := adapter.Readback(context.Background(), effect)
	if err != nil || status != setupcontract.EffectRequired {
		t.Fatalf("initial bundle readback = %s, %v", status, err)
	}
	if err := adapter.Apply(context.Background(), effect, &SecretInput{}); err != nil {
		t.Fatal(err)
	}
	status, _, err = adapter.Readback(context.Background(), effect)
	if err != nil || status != setupcontract.EffectSatisfied {
		t.Fatalf("installed bundle readback = %s, %v", status, err)
	}
}

func TestHostAdapterPersistsPATOnlyThroughSecretInput(t *testing.T) {
	layout, err := workflowhome.Resolve(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	adapter := HostAdapter{Layout: layout}
	effect := setupcontract.Effect{ID: "pat", Kind: "github_pat", Subject: layout.CredentialFile, Action: "persist", Parameters: map[string]string{"input": "stdin", "owner": "owner", "required_scopes": "repo,workflow", "fingerprint_sha256": patFingerprint("ghp_secret"), "release_manifest_digest": repeat("a", 64), "platform_setup_contract_digest": repeat("b", 64), "workflow_cli_sha256": repeat("c", 64), "release_bundled_files_digest": repeat("d", 64)}}
	status, _, err := adapter.Readback(context.Background(), effect)
	if err != nil || status != setupcontract.EffectRequired {
		t.Fatalf("initial=%s %v", status, err)
	}
	input := &SecretInput{Reader: bytes.NewBufferString("ghp_secret\n")}
	if err := adapter.Apply(context.Background(), effect, input); err != nil {
		t.Fatal(err)
	}
	status, _, err = adapter.Readback(context.Background(), effect)
	if err != nil || status != setupcontract.EffectSatisfied {
		t.Fatalf("readback=%s %v", status, err)
	}
	token, err := credential.NewFileStore(layout.CredentialFile).Get(context.Background(), credential.GatewayTarget)
	if err != nil || token != "ghp_secret" {
		t.Fatalf("token=%q %v", token, err)
	}
}

func TestEngineRejectsPlanSelectedGitHubEndpointBeforeSecretOrNetwork(t *testing.T) {
	layout, err := workflowhome.Resolve(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer server.Close()
	plan := setupcontract.Plan{SchemaVersion: 1, PlanID: "untrusted-github-endpoint", Kind: setupcontract.PlatformBootstrap, Target: setupcontract.Target{WorkflowHome: layout.Root}, Preconditions: []setupcontract.Precondition{{ID: "release", Kind: "platform_release", Subject: "v1", Expected: repeat("a", 64)}}, Effects: []setupcontract.Effect{{ID: "pat", Kind: "github_pat", Subject: layout.CredentialFile, Action: "persist", Parameters: map[string]string{"input": "stdin", "owner": "owner", "required_scopes": "repo,workflow", "fingerprint_sha256": patFingerprint("ghp_must_not_be_read"), "api_base": server.URL, "release_manifest_digest": repeat("a", 64), "platform_setup_contract_digest": repeat("b", 64), "workflow_cli_sha256": repeat("c", 64), "release_bundled_files_digest": repeat("d", 64)}}}, ExpectedResults: []setupcontract.ExpectedResult{{ID: "ready", Kind: "platform_readiness", Subject: layout.Root, Expected: "ready"}}}
	raw, _ := json.Marshal(plan)
	secret := &SecretInput{Reader: bytes.NewBufferString("ghp_must_not_be_read")}
	_, applyErr := (&Engine{Adapter: HostAdapter{Layout: layout}, SecretInput: secret, GitHubCredentialVerifier: &githubcredential.Verifier{APIBase: server.URL}}).Apply(context.Background(), raw, repeat("0", 64))
	if applyErr == nil || !strings.Contains(applyErr.Error(), `unknown parameter "api_base"`) {
		t.Fatalf("untrusted endpoint error=%v", applyErr)
	}
	if secret.consumed || requests != 0 {
		t.Fatalf("untrusted endpoint reached secret/network: consumed=%v requests=%d", secret.consumed, requests)
	}
}

func TestEngineRejectsPATOutsideExactWorkflowHomeCredentialPathBeforeSecret(t *testing.T) {
	layout, err := workflowhome.Resolve(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	contractDigest := writeTestPlatformSetupContract(t, layout)
	plan := setupcontract.Plan{SchemaVersion: 1, PlanID: "wrong-pat-path", Kind: setupcontract.PlatformBootstrap, Target: setupcontract.Target{WorkflowHome: layout.Root}, Preconditions: []setupcontract.Precondition{{ID: "release", Kind: "platform_release", Subject: "v1", Expected: repeat("a", 64)}}, Effects: []setupcontract.Effect{{ID: "pat", Kind: "github_pat", Subject: filepath.Join(t.TempDir(), "github.pat"), Action: "persist", Parameters: map[string]string{"input": "stdin", "owner": "owner", "required_scopes": "repo,workflow", "fingerprint_sha256": patFingerprint("ghp_must_not_be_read"), "release_manifest_digest": repeat("a", 64), "platform_setup_contract_digest": repeat("b", 64), "workflow_cli_sha256": repeat("c", 64), "release_bundled_files_digest": repeat("d", 64)}}}, ExpectedResults: []setupcontract.ExpectedResult{{ID: "ready", Kind: "platform_readiness", Subject: layout.Root, Expected: "ready"}}}
	plan.Effects[0].Parameters["platform_setup_contract_digest"] = contractDigest
	encoded, _ := json.Marshal(plan)
	_, raw, digest, err := setupcontract.ParsePlan(encoded)
	if err != nil {
		t.Fatal(err)
	}
	secret := &SecretInput{Reader: bytes.NewBufferString("ghp_must_not_be_read")}
	_, applyErr := (&Engine{Adapter: HostAdapter{Layout: layout}, SecretInput: secret}).Apply(context.Background(), raw, digest)
	if applyErr == nil || !strings.Contains(applyErr.Error(), "exact Workflow Home credential path") || secret.consumed {
		t.Fatalf("wrong credential path error=%v consumed=%v", applyErr, secret.consumed)
	}
}

func TestEngineRejectsPATScopesNotBoundToInstalledSignedContractBeforeSecret(t *testing.T) {
	layout, err := workflowhome.Resolve(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	_ = writeTestPlatformSetupContract(t, layout)
	plan := setupcontract.Plan{SchemaVersion: 1, PlanID: "wrong-pat-contract", Kind: setupcontract.PlatformBootstrap, Target: setupcontract.Target{WorkflowHome: layout.Root}, Preconditions: []setupcontract.Precondition{{ID: "release", Kind: "platform_release", Subject: "v1", Expected: repeat("a", 64)}}, Effects: []setupcontract.Effect{{ID: "pat", Kind: "github_pat", Subject: layout.CredentialFile, Action: "persist", Parameters: map[string]string{"input": "stdin", "owner": "owner", "required_scopes": "repo,workflow", "fingerprint_sha256": patFingerprint("ghp_must_not_be_read"), "release_manifest_digest": repeat("a", 64), "platform_setup_contract_digest": repeat("b", 64), "workflow_cli_sha256": repeat("c", 64), "release_bundled_files_digest": repeat("d", 64)}}}, ExpectedResults: []setupcontract.ExpectedResult{{ID: "ready", Kind: "platform_readiness", Subject: layout.Root, Expected: "ready"}}}
	encoded, _ := json.Marshal(plan)
	_, raw, digest, err := setupcontract.ParsePlan(encoded)
	if err != nil {
		t.Fatal(err)
	}
	secret := &SecretInput{Reader: bytes.NewBufferString("ghp_must_not_be_read")}
	_, applyErr := (&Engine{Adapter: HostAdapter{Layout: layout}, SecretInput: secret}).Apply(context.Background(), raw, digest)
	if applyErr == nil || !strings.Contains(applyErr.Error(), "exact signed Platform Setup Contract") || secret.consumed {
		t.Fatalf("wrong signed contract error=%v consumed=%v", applyErr, secret.consumed)
	}
}

func writeTestPlatformSetupContract(t *testing.T, layout workflowhome.Layout) string {
	t.Helper()
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	contract := platformrelease.PlatformSetupContract{WorkflowHomeDefault: `%LOCALAPPDATA%\AgentWorkflow`, Credential: platformrelease.CredentialContract{Kind: "classic-pat", RequiredScopes: []string{"repo", "workflow"}, OwnerBinding: "single-owner", PlaintextRelativePath: `state\credentials\github.pat`}, Docker: platformrelease.DockerDependency{Version: "4.45.0", InstallerURL: "https://example.test/docker.exe", WindowsAMD64SHA256: repeat("6", 64)}, Worker: platformrelease.WorkerPin{Image: "ghcr.io/owner/worker@sha256:" + repeat("7", 64)}, SkillBundle: platformrelease.SkillBundleContract{Version: "2.0.0", InstallScope: "user", ManagedSkills: []string{"implement"}}, RepositoryContract: platformrelease.RepositoryContractPin{Version: "1", ManifestPath: ".workflow/repository.json", CheckName: "workflow-contract", Labels: []platformrelease.RepositoryLabel{{Name: "workflow:plan", Color: "0e8a16", Description: "plan"}}}}
	raw, _ := json.Marshal(contract)
	canonical, digest, err := setupcontract.Canonicalize(raw)
	if err != nil || contract.Validate() != nil {
		t.Fatalf("test Platform Setup Contract: digest=%s err=%v", digest, err)
	}
	if err := os.WriteFile(filepath.Join(layout.Config, "platform-setup-contract.json"), canonical, 0o600); err != nil {
		t.Fatal(err)
	}
	return digest
}

func TestEngineRejectsPATFingerprintMismatchBeforeMutationOrNetwork(t *testing.T) {
	layout, err := workflowhome.Resolve(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	contractDigest := writeTestPlatformSetupContract(t, layout)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer server.Close()
	plan := setupcontract.Plan{SchemaVersion: 1, PlanID: "pat-fingerprint-fence", Kind: setupcontract.PlatformBootstrap, Target: setupcontract.Target{WorkflowHome: layout.Root}, Preconditions: []setupcontract.Precondition{{ID: "release", Kind: "platform_release", Subject: "v1", Expected: repeat("a", 64)}}, Effects: []setupcontract.Effect{{ID: "pat", Kind: "github_pat", Subject: layout.CredentialFile, Action: "persist", Parameters: map[string]string{"input": "stdin", "owner": "owner", "required_scopes": "repo,workflow", "fingerprint_sha256": patFingerprint("ghp_approved"), "release_manifest_digest": repeat("a", 64), "platform_setup_contract_digest": contractDigest, "workflow_cli_sha256": repeat("c", 64), "release_bundled_files_digest": repeat("d", 64)}}}, ExpectedResults: []setupcontract.ExpectedResult{{ID: "ready", Kind: "platform_readiness", Subject: layout.Root, Expected: "ready"}}}
	raw, _ := json.Marshal(plan)
	_, canonical, digest, err := setupcontract.ParsePlan(raw)
	if err != nil {
		t.Fatal(err)
	}
	secret := &SecretInput{Reader: bytes.NewBufferString("ghp_different\n")}
	_, applyErr := (&Engine{Adapter: HostAdapter{Layout: layout}, SecretInput: secret, GitHubCredentialVerifier: &githubcredential.Verifier{APIBase: server.URL}}).Apply(context.Background(), canonical, digest)
	if applyErr == nil || !strings.Contains(applyErr.Error(), "fingerprint differs") || requests != 0 {
		t.Fatalf("fingerprint fence err=%v requests=%d", applyErr, requests)
	}
	if _, err := os.Stat(layout.CredentialFile); !os.IsNotExist(err) {
		t.Fatalf("mismatched PAT mutated credential file: %v", err)
	}
}

func TestEnginePersistsOnlyVerifiedPATMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-OAuth-Scopes", "repo, workflow")
		_, _ = w.Write([]byte(`{"login":"owner","id":7}`))
	}))
	defer server.Close()
	layout, err := workflowhome.Resolve(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	contractDigest := writeTestPlatformSetupContract(t, layout)
	plan := setupcontract.Plan{SchemaVersion: 1, PlanID: "pat-plan", Kind: setupcontract.PlatformBootstrap, Target: setupcontract.Target{WorkflowHome: layout.Root}, Preconditions: []setupcontract.Precondition{{ID: "release", Kind: "platform_release", Subject: "v1", Expected: repeat("a", 64)}}, Effects: []setupcontract.Effect{{ID: "pat", Kind: "github_pat", Subject: layout.CredentialFile, Action: "persist", Parameters: map[string]string{"input": "stdin", "owner": "owner", "required_scopes": "repo,workflow", "fingerprint_sha256": patFingerprint("ghp_secret"), "release_manifest_digest": repeat("a", 64), "platform_setup_contract_digest": repeat("b", 64), "workflow_cli_sha256": repeat("c", 64), "release_bundled_files_digest": repeat("d", 64)}}}, ExpectedResults: []setupcontract.ExpectedResult{{ID: "ready", Kind: "platform_readiness", Subject: layout.Root, Expected: "ready"}}}
	plan.Effects[0].Parameters["platform_setup_contract_digest"] = contractDigest
	raw, _ := json.Marshal(plan)
	_, _, digest, err := setupcontract.ParsePlan(raw)
	if err != nil {
		t.Fatal(err)
	}
	engine := Engine{Adapter: HostAdapter{Layout: layout}, SecretInput: &SecretInput{Reader: bytes.NewBufferString("ghp_secret")}, GitHubCredentialVerifier: &githubcredential.Verifier{APIBase: server.URL}, ExpectedResultVerifier: passingExpectedResultVerifier, PlatformPreconditionVerifier: passingPlatformPreconditionVerifier}
	result, err := engine.Apply(context.Background(), raw, digest)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != setupcontract.ExecutionSucceeded {
		t.Fatalf("result=%#v", result)
	}
	db, err := store.Open(context.Background(), filepath.Join(layout.State, "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	verification, err := db.GitHubPATVerification(context.Background())
	if err != nil || verification.Login != "owner" {
		t.Fatalf("verification=%#v %v", verification, err)
	}
}

func TestEngineReplacesPersistedPATThatFailsLiveVerification(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ghp_replacement" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
			return
		}
		w.Header().Set("X-OAuth-Scopes", "repo, workflow")
		_, _ = w.Write([]byte(`{"login":"owner","id":7}`))
	}))
	defer server.Close()
	layout, err := workflowhome.Resolve(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	contractDigest := writeTestPlatformSetupContract(t, layout)
	if err := credential.NewFileStore(layout.CredentialFile).Set(context.Background(), credential.GatewayTarget, "ghp_revoked"); err != nil {
		t.Fatal(err)
	}
	plan := setupcontract.Plan{SchemaVersion: 1, PlanID: "replace-pat", Kind: setupcontract.PlatformBootstrap, Target: setupcontract.Target{WorkflowHome: layout.Root}, Preconditions: []setupcontract.Precondition{{ID: "release", Kind: "platform_release", Subject: "v1", Expected: repeat("a", 64)}}, Effects: []setupcontract.Effect{{ID: "pat", Kind: "github_pat", Subject: layout.CredentialFile, Action: "replace", Parameters: map[string]string{"input": "stdin", "owner": "owner", "required_scopes": "repo,workflow", "fingerprint_sha256": patFingerprint("ghp_replacement"), "release_manifest_digest": repeat("a", 64), "platform_setup_contract_digest": repeat("b", 64), "workflow_cli_sha256": repeat("c", 64), "release_bundled_files_digest": repeat("d", 64)}}}, ExpectedResults: []setupcontract.ExpectedResult{{ID: "ready", Kind: "platform_readiness", Subject: layout.Root, Expected: "ready"}}}
	plan.Effects[0].Parameters["platform_setup_contract_digest"] = contractDigest
	raw, _ := json.Marshal(plan)
	_, _, digest, err := setupcontract.ParsePlan(raw)
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&Engine{Adapter: HostAdapter{Layout: layout}, SecretInput: &SecretInput{Reader: bytes.NewBufferString("ghp_replacement")}, GitHubCredentialVerifier: &githubcredential.Verifier{APIBase: server.URL}, ExpectedResultVerifier: passingExpectedResultVerifier, PlatformPreconditionVerifier: passingPlatformPreconditionVerifier}).Apply(context.Background(), raw, digest)
	if err != nil || result.Status != setupcontract.ExecutionSucceeded {
		t.Fatalf("replacement result=%#v err=%v", result, err)
	}
	token, err := credential.NewFileStore(layout.CredentialFile).Get(context.Background(), credential.GatewayTarget)
	if err != nil || token != "ghp_replacement" {
		t.Fatalf("replacement token=%q err=%v", token, err)
	}
}
