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
				"base": map[string]string{"ref": "main"},
			})
		case "/repos/owner/repo/compare/accepted-candidate...main":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ahead"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	reached, err := NewClient(server.URL, "", server.Client()).pullRequestReachedMain(context.Background(), "owner/repo", 7, "accepted-candidate")
	if err != nil {
		t.Fatal(err)
	}
	if !reached {
		t.Fatal("mapped candidate reachable from main was not delivered")
	}
}
