package onboarding

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPublishDefaultBranchScopesPATToCanonicalPushOnly(t *testing.T) {
	repo := newRepo(t)
	bare := filepath.Join(t.TempDir(), "remote.git")
	git(t, "", "clone", "--bare", repo, bare)
	canonicalURL := "https://github.com/owner/repo.git"
	capture := installCapturedGitForPushTest(t, canonicalURL, bare)
	credential := GitCredential{Username: "publisher", Token: "github_pat_publish_scope_secret"}
	if err := PublishDefaultBranch(context.Background(), repo, canonicalURL, "main", credential); err != nil {
		t.Fatal(err)
	}
	assertScopedNetworkCredential(t, capture, "push", canonicalURL, credential)
}

func TestSafeFastForwardScopesPATToCanonicalFetchOnly(t *testing.T) {
	repo := newRepo(t)
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
	canonicalURL := "https://github.com/owner/repo.git"
	capture := installCapturedGitForPushTest(t, canonicalURL, bare)
	credential := GitCredential{Username: "fetcher", Token: "github_pat_fetch_scope_secret"}
	if err := SafeFastForward(context.Background(), repo, "owner/repo", "main", preMerge, mergeHead, credential); err != nil {
		t.Fatal(err)
	}
	assertScopedNetworkCredential(t, capture, "fetch", canonicalURL, credential)
}

func installCapturedGitForPushTest(t *testing.T, canonicalURL, localRemote string) string {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	testExecutable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	shimRoot := t.TempDir()
	gitShim := filepath.Join(shimRoot, "git")
	if runtime.GOOS == "windows" {
		gitShim += ".exe"
	}
	executableData, err := os.ReadFile(testExecutable)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gitShim, executableData, 0o700); err != nil {
		t.Fatal(err)
	}
	capture := filepath.Join(t.TempDir(), "git-processes.jsonl")
	t.Setenv("WORKFLOW_TEST_REAL_GIT", realGit)
	t.Setenv("WORKFLOW_TEST_CANONICAL_URL", canonicalURL)
	t.Setenv("WORKFLOW_TEST_LOCAL_REMOTE", localRemote)
	t.Setenv("WORKFLOW_TEST_GIT_CAPTURE", capture)
	t.Setenv("PATH", shimRoot+string(os.PathListSeparator)+os.Getenv("PATH"))
	return capture
}

func assertScopedNetworkCredential(t *testing.T, capturePath, networkSubcommand, canonicalURL string, credential GitCredential) {
	t.Helper()
	encodedCredential := base64.StdEncoding.EncodeToString([]byte(credential.Username + ":" + credential.Token))
	seenNetwork := false
	for _, capture := range readCapturedProcesses(t, capturePath) {
		subcommand := capturedGitSubcommand(capture.Args)
		serialized := strings.Join(append(append([]string{}, capture.Args...), capture.Env...), "\n")
		if subcommand == networkSubcommand {
			seenNetwork = true
			for _, wanted := range []string{
				"GIT_CONFIG_KEY_0=http." + canonicalURL + ".extraHeader",
				"GIT_CONFIG_VALUE_0=Authorization: Basic " + encodedCredential,
			} {
				if !strings.Contains(serialized, wanted) {
					t.Fatalf("%s lacks scoped credential environment %q", subcommand, wanted)
				}
			}
			continue
		}
		for _, forbidden := range []string{credential.Token, encodedCredential, "Authorization: Basic", "extraHeader"} {
			if strings.Contains(serialized, forbidden) {
				t.Fatalf("local git %s received credential material %q", subcommand, forbidden)
			}
		}
	}
	if !seenNetwork {
		t.Fatalf("network operation %s was not observed", networkSubcommand)
	}
}

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
