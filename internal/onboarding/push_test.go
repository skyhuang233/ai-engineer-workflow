package onboarding

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestSafeFastForwardRejectsBranchAndPreMergeHeadDriftBeforeFetch(t *testing.T) {
	repo := newRepo(t)
	git(t, repo, "remote", "set-url", "origin", "git@github.com:owner/repo.git")
	approved := testGitOutput(t, repo, "rev-parse", "HEAD")
	git(t, repo, "checkout", "-b", "other")
	err := SafeFastForward(context.Background(), repo, "owner/repo", "main", approved, strings.Repeat("b", 40), GitCredential{Token: "secret"})
	if err == nil || !strings.Contains(err.Error(), "checked-out branch") {
		t.Fatalf("branch drift accepted: %v", err)
	}
	git(t, repo, "checkout", "main")
	err = SafeFastForward(context.Background(), repo, "owner/repo", "main", strings.Repeat("a", 40), strings.Repeat("b", 40), GitCredential{Token: "secret"})
	if err == nil || !strings.Contains(err.Error(), "pre-merge HEAD") {
		t.Fatalf("HEAD drift accepted: %v", err)
	}
	if origin := testGitOutput(t, repo, "remote", "get-url", "origin"); origin != "git@github.com:owner/repo.git" {
		t.Fatalf("origin changed during rejected authenticated fetch: %q", origin)
	}
}

func TestSafeFastForwardFetchesPersistedMergeSHAInsteadOfLatestRemoteBranch(t *testing.T) {
	repo := newRepo(t)
	git(t, repo, "remote", "set-url", "origin", "git@github.com:owner/repo.git")
	preMerge := testGitOutput(t, repo, "rev-parse", "HEAD")
	bare := filepath.Join(t.TempDir(), "remote.git")
	git(t, "", "clone", "--bare", repo, bare)
	advance := filepath.Join(t.TempDir(), "advance")
	git(t, "", "clone", bare, advance)
	git(t, advance, "config", "user.name", "Test")
	git(t, advance, "config", "user.email", "test@example.com")
	git(t, advance, "commit", "--allow-empty", "-m", "approved onboarding merge")
	mergeHead := testGitOutput(t, advance, "rev-parse", "HEAD")
	git(t, advance, "push", "origin", "main")
	git(t, advance, "commit", "--allow-empty", "-m", "unapproved later commit")
	extraHead := testGitOutput(t, advance, "rev-parse", "HEAD")
	git(t, advance, "push", "origin", "main")
	fileURL := "file:///" + strings.TrimPrefix(filepath.ToSlash(bare), "/")
	git(t, repo, "config", "url."+fileURL+".insteadOf", "https://github.com/owner/repo.git")
	if err := SafeFastForward(context.Background(), repo, "owner/repo", "main", preMerge, mergeHead, GitCredential{}); err != nil {
		t.Fatal(err)
	}
	if got := testGitOutput(t, repo, "rev-parse", "HEAD"); got != mergeHead || got == extraHead {
		t.Fatalf("local HEAD = %s, merge=%s extra=%s", got, mergeHead, extraHead)
	}
	if origin := testGitOutput(t, repo, "remote", "get-url", "origin"); origin != "git@github.com:owner/repo.git" {
		t.Fatalf("SSH origin changed to %q", origin)
	}
}
