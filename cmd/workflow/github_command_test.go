package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	workflowgithub "github.com/skyhuang233/workflow/internal/github"
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
