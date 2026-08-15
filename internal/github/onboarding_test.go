package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestActionsPolicyDiscoveryAndEnablementPreserveAllowedActions(t *testing.T) {
	var update map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo":
			_, _ = w.Write([]byte(`{"full_name":"owner/repo","default_branch":"main","has_issues":false,"permissions":{"admin":true},"allow_squash_merge":true}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/actions/permissions":
			_, _ = w.Write([]byte(`{"enabled":false,"allowed_actions":"selected"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/actions/permissions/selected-actions":
			_, _ = w.Write([]byte(`{"github_owned_allowed":true,"verified_allowed":false,"patterns_allowed":[]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/branches/main/protection":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"not found"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/rulesets":
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPatch && r.URL.Path == "/repos/owner/repo":
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodPut && r.URL.Path == "/repos/owner/repo/actions/permissions":
			if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
				t.Fatal(err)
			}
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()
	client := NewClient(server.URL, "token", server.Client()).WithRepositoryOwner("owner")
	policy, err := client.DiscoverPolicy(context.Background(), "owner/repo", "main")
	if err != nil || policy.ActionsAllowed != "selected" || !policy.GitHubOwnedActionsAllowed {
		t.Fatalf("policy=%#v err=%v", policy, err)
	}
	if err := client.UpdateRepositoryFeatures(context.Background(), "owner/repo", true, true, policy.ActionsAllowed); err != nil {
		t.Fatal(err)
	}
	if update["allowed_actions"] != "selected" {
		t.Fatalf("Actions update = %#v", update)
	}
}

func TestActionsPolicyDiscoveryRejectsPoliciesThatCannotRunGitHubCheckout(t *testing.T) {
	for _, test := range []struct {
		name, allowed, selected string
	}{
		{name: "local only", allowed: "local_only"},
		{name: "selected without GitHub owned actions", allowed: "selected", selected: `{"github_owned_allowed":false}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/repos/owner/repo":
					_, _ = w.Write([]byte(`{"full_name":"owner/repo","default_branch":"main","has_issues":true,"permissions":{"admin":true},"allow_squash_merge":true}`))
				case "/repos/owner/repo/actions/permissions":
					_, _ = w.Write([]byte(`{"enabled":true,"allowed_actions":"` + test.allowed + `"}`))
				case "/repos/owner/repo/actions/permissions/selected-actions":
					_, _ = w.Write([]byte(test.selected))
				default:
					t.Fatalf("policy discovery continued to %s", r.URL.String())
				}
			}))
			defer server.Close()
			_, err := NewClient(server.URL, "token", server.Client()).DiscoverPolicy(context.Background(), "owner/repo", "main")
			if err == nil || !strings.Contains(err.Error(), "checkout") {
				t.Fatalf("Actions policy accepted without checkout: %v", err)
			}
		})
	}
}

func TestActionsEnablementRejectsUnplannedAllowedActions(t *testing.T) {
	client := NewClient("https://api.github.test", "token", nil)
	if err := client.UpdateRepositoryFeatures(context.Background(), "owner/repo", true, true, ""); err == nil {
		t.Fatal("empty Actions policy was silently widened")
	}
}

func TestOrganizationPublicationPreflightFailsClosed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/acme/repo":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"not found"}`))
		case "/orgs/acme":
			_, _ = w.Write([]byte(`{"login":"acme","members_can_create_repositories":true,"members_can_create_private_repositories":false}`))
		case "/user/memberships/orgs/acme":
			_, _ = w.Write([]byte(`{"state":"active","role":"member"}`))
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	err := NewClient(server.URL, "token", server.Client()).PreflightCreateRepository(context.Background(), "acme", "alice", "repo", true)
	if err == nil || !strings.Contains(err.Error(), "private") {
		t.Fatalf("private organization publication preflight = %v", err)
	}
}

func TestOrganizationPublicationPreflightRequiresProvableActionsAndMergePolicy(t *testing.T) {
	tests := []struct {
		name        string
		actionsCode int
		actions     string
		rulesets    string
		want        string
	}{
		{name: "Actions discovery missing", actionsCode: http.StatusForbidden, actions: `{"message":"forbidden"}`, rulesets: `[]`, want: "Actions policy"},
		{name: "new repositories excluded", actionsCode: http.StatusOK, actions: `{"enabled_repositories":"selected","allowed_actions":"all"}`, rulesets: `[]`, want: "new repository"},
		{name: "required actions not provable", actionsCode: http.StatusOK, actions: `{"enabled_repositories":"all","allowed_actions":"selected"}`, rulesets: `[]`, want: "required onboarding actions"},
		{name: "review required", actionsCode: http.StatusOK, actions: `{"enabled_repositories":"all","allowed_actions":"all"}`, rulesets: `[{"enforcement":"active","rules":[{"type":"pull_request","parameters":{"required_approving_review_count":1}}]}]`, want: "human review"},
		{name: "merge queue required", actionsCode: http.StatusOK, actions: `{"enabled_repositories":"all","allowed_actions":"all"}`, rulesets: `[{"enforcement":"active","rules":[{"type":"merge_queue"}]}]`, want: "merge queue"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/repos/acme/repo":
					w.WriteHeader(http.StatusNotFound)
					_, _ = w.Write([]byte(`{"message":"not found"}`))
				case "/orgs/acme":
					_, _ = w.Write([]byte(`{"login":"acme","members_can_create_repositories":true,"members_can_create_private_repositories":true}`))
				case "/user/memberships/orgs/acme":
					_, _ = w.Write([]byte(`{"state":"active","role":"admin"}`))
				case "/orgs/acme/actions/permissions":
					w.WriteHeader(test.actionsCode)
					_, _ = w.Write([]byte(test.actions))
				case "/orgs/acme/actions/permissions/selected-actions":
					_, _ = w.Write([]byte(`{"github_owned_allowed":false}`))
				case "/orgs/acme/rulesets":
					_, _ = w.Write([]byte(test.rulesets))
				default:
					t.Fatalf("unexpected %s %s", r.Method, r.URL.String())
				}
			}))
			defer server.Close()
			err := NewClient(server.URL, "token", server.Client()).PreflightCreateRepository(context.Background(), "acme", "alice", "repo", true)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("preflight error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestOnboardingRepositoryAndPullRequestMutations(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/user/repos":
			_, _ = w.Write([]byte(`{"full_name":"owner/repo","default_branch":"main","private":true,"has_issues":true,"permissions":{"admin":true}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/repos/owner/repo/labels":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"name":"workflow:plan"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/repos/owner/repo/pulls":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"number":7,"head":{"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}`))
		case r.Method == http.MethodPut && r.URL.Path == "/repos/owner/repo/pulls/7/merge":
			_, _ = w.Write([]byte(`{"merged":true,"sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}`))
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	client := NewClient(server.URL, "token", server.Client()).WithRepositoryOwner("owner")
	repo, err := client.CreateRepository(context.Background(), "owner", "owner", "repo", true)
	if err != nil || repo.FullName != "owner/repo" {
		t.Fatalf("repo=%#v %v", repo, err)
	}
	if err := client.CreateLabel(context.Background(), "owner/repo", ManagedLabel{Name: "workflow:plan", Color: "123456", Description: "plan"}); err != nil {
		t.Fatal(err)
	}
	pr, err := client.CreateOnboardingPullRequest(context.Background(), "owner/repo", PullRequestCreate{Title: "Onboard", Head: "workflow/onboard", Base: "main", Body: "digest"})
	if err != nil || pr.Number != 7 {
		t.Fatalf("pr=%#v %v", pr, err)
	}
	merge, err := client.MergeOnboardingPullRequest(context.Background(), "owner/repo", 7, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "squash")
	if err != nil || !merge.Merged {
		t.Fatalf("merge=%#v %v", merge, err)
	}
}

func TestFindOnboardingPullRequestUsesDigestBoundBranchIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/repos/owner/repo/pulls" || r.URL.Query().Get("state") != "all" || r.URL.Query().Get("head") != "owner:workflow/onboarding-digest" || r.URL.Query().Get("base") != "main" {
			t.Fatalf("request=%s %s", r.Method, r.URL.String())
		}
		_, _ = w.Write([]byte(`[{"number":7,"body":"Approved Setup Plan SHA-256: digest","head":{"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","ref":"workflow/onboarding-digest"}}]`))
	}))
	defer server.Close()
	pull, found, err := NewClient(server.URL, "token", server.Client()).FindOnboardingPullRequest(context.Background(), "owner/repo", "owner", "workflow/onboarding-digest", "main")
	if err != nil || !found || pull.Number != 7 {
		t.Fatalf("pull=%#v found=%t err=%v", pull, found, err)
	}
}
