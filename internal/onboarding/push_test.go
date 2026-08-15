package onboarding

import (
	"context"
	"strings"
	"testing"
)

func TestSafeFastForwardRejectsBranchAndPreMergeHeadDriftBeforeFetch(t *testing.T) {
	repo := newRepo(t)
	git(t, repo, "remote", "set-url", "origin", "git@github.com:owner/repo.git")
	approved := testGitOutput(t, repo, "rev-parse", "HEAD")
	git(t, repo, "checkout", "-b", "other")
	err := SafeFastForward(context.Background(), repo, "owner/repo", "main", approved, GitCredential{Token: "secret"})
	if err == nil || !strings.Contains(err.Error(), "checked-out branch") {
		t.Fatalf("branch drift accepted: %v", err)
	}
	git(t, repo, "checkout", "main")
	err = SafeFastForward(context.Background(), repo, "owner/repo", "main", strings.Repeat("a", 40), GitCredential{Token: "secret"})
	if err == nil || !strings.Contains(err.Error(), "pre-merge HEAD") {
		t.Fatalf("HEAD drift accepted: %v", err)
	}
	if origin := testGitOutput(t, repo, "remote", "get-url", "origin"); origin != "git@github.com:owner/repo.git" {
		t.Fatalf("origin changed during rejected authenticated fetch: %q", origin)
	}
}
