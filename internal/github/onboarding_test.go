package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	if err != nil || policy.ActionsAllowed != "selected" {
		t.Fatalf("policy=%#v err=%v", policy, err)
	}
	if err := client.UpdateRepositoryFeatures(context.Background(), "owner/repo", true, true, policy.ActionsAllowed); err != nil {
		t.Fatal(err)
	}
	if update["allowed_actions"] != "selected" {
		t.Fatalf("Actions update = %#v", update)
	}
}

func TestActionsEnablementRejectsUnplannedAllowedActions(t *testing.T) {
	client := NewClient("https://api.github.test", "token", nil)
	if err := client.UpdateRepositoryFeatures(context.Background(), "owner/repo", true, true, ""); err == nil {
		t.Fatal("empty Actions policy was silently widened")
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
		if r.Method != http.MethodGet || r.URL.Path != "/repos/owner/repo/pulls" || r.URL.Query().Get("head") != "owner:workflow/onboarding-digest" || r.URL.Query().Get("base") != "main" {
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
