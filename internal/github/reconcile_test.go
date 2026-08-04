package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPullRequestReachedMainRequiresMappedCandidateReachability(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/owner/repo/pulls/7":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"state": "closed", "merged_at": "2026-07-31T00:00:00Z", "merge_commit_sha": "merged-other-revision",
				"merged_by": map[string]string{"login": "owner", "type": "User"},
				"base":      map[string]string{"ref": "main"},
			})
		case "/repos/owner/repo/compare/accepted-candidate...main":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ahead"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	reached, err := NewClient(server.URL, "", server.Client()).WithRepositoryOwner("owner").pullRequestReachedMain(context.Background(), "owner/repo", 7, "accepted-candidate")
	if err != nil {
		t.Fatal(err)
	}
	if !reached {
		t.Fatal("mapped candidate reachable from main was not delivered")
	}
}

func TestPullRequestReachedMainRejectsNonOwnerAndBotMergers(t *testing.T) {
	for _, mergedBy := range []map[string]string{{"login": "reviewer", "type": "User"}, {"login": "owner[bot]", "type": "Bot"}} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/repos/owner/repo/pulls/7" {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"state": "closed", "merged_at": "2026-07-31T00:00:00Z", "merge_commit_sha": "merged", "merged_by": mergedBy,
				"base": map[string]string{"ref": "main"},
			})
		}))
		reached, err := NewClient(server.URL, "", server.Client()).WithRepositoryOwner("owner").pullRequestReachedMain(context.Background(), "owner/repo", 7, "accepted-candidate")
		server.Close()
		if err != nil || reached {
			t.Fatalf("merged_by=%#v reached=%t err=%v", mergedBy, reached, err)
		}
	}
}

func TestClosedUnmergedPullRequestFreezesInsteadOfDelivering(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/owner/repo/pulls/7" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"state": "closed", "merged_at": nil, "merge_commit_sha": nil, "base": map[string]string{"ref": "main"}})
	}))
	defer server.Close()
	state, err := NewClient(server.URL, "", server.Client()).pullRequestDeliveryState(context.Background(), "owner/repo", 7, "candidate")
	if err != nil {
		t.Fatal(err)
	}
	if state != pullRequestClosedUnmerged {
		t.Fatalf("state = %d, want closed-unmerged", state)
	}
}
