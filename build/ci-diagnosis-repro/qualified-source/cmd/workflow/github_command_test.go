package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skyhuang233/workflow/internal/credential"
	workflowgithub "github.com/skyhuang233/workflow/internal/github"
	"github.com/skyhuang233/workflow/internal/store"
	"github.com/skyhuang233/workflow/internal/workflowhome"
)

func TestManagedGitHubIssueListPaginatesWithoutExposingCredential(t *testing.T) {
	const token = "github_pat_managed_secret"
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("authorization = %q", got)
		}
		if r.URL.Path != "/repos/owner/repo/issues" || r.URL.Query().Get("per_page") != "100" {
			t.Errorf("request = %s", r.URL.String())
		}
		requests++
		count := 1
		if requests == 1 {
			count = 100
		}
		items := make([]map[string]any, count)
		for index := range items {
			items[index] = map[string]any{"number": (requests-1)*100 + index + 1}
		}
		_ = json.NewEncoder(w).Encode(items)
	}))
	defer server.Close()
	client := workflowgithub.NewClient(server.URL, token, server.Client()).WithRepositoryOwner("owner")
	var output bytes.Buffer
	if err := executeManagedGitHub(context.Background(), client, "owner/repo", "issue-list", managedGitHubOptions{State: "all"}, &output); err != nil {
		t.Fatal(err)
	}
	var items []map[string]any
	if err := json.Unmarshal(output.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 101 || requests != 2 {
		t.Fatalf("items=%d requests=%d", len(items), requests)
	}
	if strings.Contains(output.String(), token) {
		t.Fatal("managed GitHub output leaked the PAT")
	}
}

func TestManagedGitHubUsesNativeRelationshipEndpointsAndRejectsUnknownWrites(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		if r.Method == http.MethodPost {
			var body map[string]int64
			_ = json.NewDecoder(r.Body).Decode(&body)
			if len(body) != 1 || body["sub_issue_id"] != 91 && body["issue_id"] != 91 {
				t.Errorf("relationship body = %#v", body)
			}
		}
		fmt.Fprint(w, `{}`)
	}))
	defer server.Close()
	client := workflowgithub.NewClient(server.URL, "secret", server.Client()).WithRepositoryOwner("owner")
	for _, operation := range []string{"subissues-add", "blocked-by-add"} {
		if err := executeManagedGitHub(context.Background(), client, "owner/repo", operation, managedGitHubOptions{Number: 7, Related: 91}, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"POST /repos/owner/repo/issues/7/sub_issues", "POST /repos/owner/repo/issues/7/dependencies/blocked_by"}
	if fmt.Sprint(paths) != fmt.Sprint(want) {
		t.Fatalf("paths=%v want=%v", paths, want)
	}
	if err := executeManagedGitHub(context.Background(), client, "owner/repo", "merge", managedGitHubOptions{}, &bytes.Buffer{}); err == nil {
		t.Fatal("unapproved GitHub operation was accepted")
	}
	if err := executeManagedGitHub(context.Background(), client, "owner/repo", "inbox-answer", managedGitHubOptions{Number: 7}, &bytes.Buffer{}); err == nil {
		t.Fatal("managed GitHub comment path bypassed atomic Workflow Inbox answering")
	}
}

func TestManagedGitHubIdentityDistinguishesOrganizationOwnerFromPATLoginWithoutSecret(t *testing.T) {
	var output bytes.Buffer
	identity := managedGitHubIdentity{Login: "alice", UserID: 42, Type: "User", Repository: "acme/repo", Owner: "acme"}
	if err := writeManagedGitHubIdentity(&output, identity); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); !strings.Contains(got, `"login":"alice"`) || !strings.Contains(got, `"owner":"acme"`) || strings.Contains(got, "token") {
		t.Fatalf("managed identity output = %s", got)
	}
}

func TestManagedGitHubIdentityUsesWorkflowHomePATAndAdmittedCanonicalRepository(t *testing.T) {
	const token = "github_pat_identity_secret"
	layout, err := workflowhome.Resolve(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(context.Background(), filepath.Join(layout.State, "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	now := time.Now().UTC()
	if err := database.RecordRepositoryAdmission(context.Background(), store.RepositoryAdmission{Repository: "alice/repo", OnboardingPlanDigestSHA256: strings.Repeat("a", 64), ContractVersion: "1", ManifestDigestSHA256: strings.Repeat("b", 64), Eligible: true, VerifiedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := credential.NewFileStore(layout.CredentialFile).Set(context.Background(), credential.GatewayTarget, token); err != nil {
		t.Fatal(err)
	}
	if err := database.RecordGitHubPATVerification(context.Background(), store.GitHubPATVerification{FingerprintSHA256: credential.Fingerprint(token), Login: "alice", UserID: 42, Owner: "alice", Scopes: []string{"repo", "workflow"}, CredentialPath: layout.CredentialFile, Status: "verified", VerifiedAt: now}); err != nil {
		t.Fatal(err)
	}
	repository := t.TempDir()
	for _, args := range [][]string{{"init"}, {"remote", "add", "origin", "git@github.com:alice/repo.git"}} {
		command := exec.Command("git", append([]string{"-C", repository}, args...)...)
		if output, runErr := command.CombinedOutput(); runErr != nil {
			t.Fatalf("git %v: %v: %s", args, runErr, output)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Fatal("managed identity request omitted Workflow Home PAT")
		}
		w.Header().Set("X-OAuth-Scopes", "repo, workflow")
		switch r.URL.Path {
		case "/user":
			fmt.Fprint(w, `{"login":"alice","id":42,"type":"User"}`)
		case "/repos/alice/repo":
			fmt.Fprint(w, `{"full_name":"alice/repo","private":true,"owner":{"login":"alice"}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	session, err := managedGitHubClient(context.Background(), database, layout, repository, server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "remote.origin.url")
	t.Setenv("GIT_CONFIG_VALUE_0", "https://github.com/attacker/redirected.git")
	if _, err := managedGitHubClient(context.Background(), database, layout, repository, server.URL, server.Client()); err != nil {
		t.Fatalf("process Git environment redirected workflow github origin: %v", err)
	}
	t.Setenv("GIT_CONFIG_COUNT", "0")
	command := exec.Command("git", "-C", repository, "config", "--local", "url.https://attacker.invalid/.insteadOf", "https://github.com/")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("configure local origin redirect: %v: %s", err, output)
	}
	if _, err := managedGitHubClient(context.Background(), database, layout, repository, server.URL, server.Client()); err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("repository-owned Git URL redirect was not rejected as unsafe: %v", err)
	}
	var output strings.Builder
	if err := writeManagedGitHubIdentity(&output, session.Identity); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); !strings.Contains(got, `"repository":"alice/repo"`) || !strings.Contains(got, `"type":"User"`) || strings.Contains(got, token) {
		t.Fatalf("identity output = %s", got)
	}
	if _, err := os.Stat(filepath.Join(repository, ".git", "index.lock")); !os.IsNotExist(err) {
		t.Fatalf("managed identity created a Git index lock: %v", err)
	}
}
