package onboarding

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestPublishDefaultBranchScopesPATToCanonicalPushOnly(t *testing.T) {
	repo := newRepo(t)
	bare := filepath.Join(t.TempDir(), "remote.git")
	git(t, "", "clone", "--bare", repo, bare)
	canonicalURL := "https://github.com/owner/repo.git"
	capture := installCapturedGitForPushTest(t, canonicalURL, bare)
	hookCapture := filepath.Join(t.TempDir(), "pre-push-environment")
	hook := filepath.Join(repo, ".git", "hooks", "pre-push")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nenv > \"$WORKFLOW_TEST_PRE_PUSH_CAPTURE\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WORKFLOW_TEST_PRE_PUSH_CAPTURE", hookCapture)
	credential := GitCredential{Username: "publisher", Token: "github_pat_publish_scope_secret"}
	if err := PublishDefaultBranch(context.Background(), repo, canonicalURL, "main", credential); err != nil {
		t.Fatal(err)
	}
	assertScopedNetworkCredential(t, capture, canonicalURL, credential, "push")
	for name, path := range map[string]string{"pre-push hook": hookCapture} {
		if data, err := os.ReadFile(path); err == nil {
			t.Fatalf("untrusted %s executed with authenticated push environment: %s", name, data)
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
}

func TestAuthenticatedGitRejectsRepositoryLocalTransportOverridesWithoutNetwork(t *testing.T) {
	for _, test := range []struct{ name, key, value string }{
		{name: "proxy", key: "http.proxy"},
		{name: "ssl verify", key: "http.sslVerify", value: "false"},
		{name: "extra header", key: "http.extraHeader", value: "Authorization: Basic attacker"},
		{name: "credential helper", key: "credential.helper", value: "!echo attacker"},
		{name: "instead of", key: "url.http://127.0.0.1/.insteadOf", value: "https://github.com/"},
		{name: "remote helper", key: "remote.origin.vcs", value: "ext"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var requests int
			server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
			defer server.Close()
			repo := newRepo(t)
			value := test.value
			if value == "" {
				value = server.URL
			}
			git(t, repo, "config", test.key, value)
			token := "github_pat_round14_must_never_reach_transport"
			err := PublishDefaultBranch(context.Background(), repo, "https://github.com/owner/repo.git", "main", GitCredential{Username: "x-access-token", Token: token})
			if err == nil || !strings.Contains(err.Error(), "unsafe") || requests != 0 {
				t.Fatalf("unsafe local transport config was used: requests=%d err=%v", requests, err)
			}
		})
	}
}

func TestSafeFastForwardRejectsDirtyTreeWithoutMutation(t *testing.T) {
	repo := newRepo(t)
	before := testGitOutput(t, repo, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("dirty\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := SafeFastForward(context.Background(), repo, "owner/repo", "main", before, strings.Repeat("b", 40), GitCredential{Token: "github_pat_not_used"})
	if err == nil {
		t.Fatal("dirty repository was fast-forwarded")
	}
	if after := testGitOutput(t, repo, "rev-parse", "HEAD"); after != before {
		t.Fatalf("dirty rejection mutated HEAD from %s to %s", before, after)
	}
	data, readErr := os.ReadFile(filepath.Join(repo, "README.md"))
	if readErr != nil || string(data) != "dirty\n" {
		t.Fatalf("dirty rejection mutated worktree: %q %v", data, readErr)
	}
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
	assertScopedNetworkCredential(t, capture, canonicalURL, credential, "fetch")
}

func TestDeleteRemoteBranchWithLeaseAtomicallyRejectsConcurrentAdvance(t *testing.T) {
	repo := newRepo(t)
	approvedHead := testGitOutput(t, repo, "rev-parse", "HEAD")
	git(t, repo, "commit", "--allow-empty", "-m", "external advance")
	externalHead := testGitOutput(t, repo, "rev-parse", "HEAD")
	bare := filepath.Join(t.TempDir(), "remote.git")
	git(t, "", "clone", "--bare", repo, bare)
	branch := "workflow/onboarding-aaaaaaaaaaaa"
	git(t, "", "--git-dir", bare, "update-ref", "refs/heads/"+branch, approvedHead)
	canonicalURL := "https://github.com/owner/repo.git"
	capture := installCapturedGitForPushTest(t, canonicalURL, bare)
	t.Setenv("WORKFLOW_TEST_ADVANCE_REMOTE_REF", "refs/heads/"+branch)
	t.Setenv("WORKFLOW_TEST_ADVANCE_REMOTE_SHA", externalHead)
	credential := GitCredential{Username: "x-access-token", Token: "github_pat_cleanup_atomic_secret"}
	err := DeleteRemoteBranchWithLease(context.Background(), "owner/repo", branch, approvedHead, credential)
	if err == nil {
		t.Fatal("concurrently advanced cleanup branch was deleted")
	}
	if got := testGitOutput(t, "", "--git-dir", bare, "rev-parse", "refs/heads/"+branch); got != externalHead {
		t.Fatalf("atomic cleanup changed concurrent head: got %s want %s", got, externalHead)
	}
	assertScopedNetworkCredential(t, capture, canonicalURL, credential, "ls-remote", "push")
	wantedLease := "--force-with-lease=refs/heads/" + branch + ":" + approvedHead
	seenLease := false
	for _, process := range readCapturedProcesses(t, capture) {
		if capturedGitSubcommand(process.Args) == "push" && slices.Contains(process.Args, wantedLease) && slices.Contains(process.Args, ":refs/heads/"+branch) {
			seenLease = true
		}
	}
	if !seenLease {
		t.Fatalf("cleanup push did not carry exact expected-OID lease: %#v", readCapturedProcesses(t, capture))
	}
}

func TestSafeFastForwardRejectsWorktreeChangeDuringFetch(t *testing.T) {
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
	installCapturedGitForPushTest(t, "https://github.com/owner/repo.git", bare)
	dirtyPath := filepath.Join(repo, "README.md")
	t.Setenv("WORKFLOW_TEST_DIRTY_AFTER_FETCH", dirtyPath)
	err := SafeFastForward(context.Background(), repo, "owner/repo", "main", preMerge, mergeHead, GitCredential{})
	if err == nil || !strings.Contains(err.Error(), "working tree changed") {
		t.Fatalf("fetch-time dirty drift accepted: %v", err)
	}
	if got := testGitOutput(t, repo, "rev-parse", "HEAD"); got != preMerge {
		t.Fatalf("dirty drift mutated HEAD to %s", got)
	}
	if data, _ := os.ReadFile(dirtyPath); string(data) != "user edit during fetch\n" {
		t.Fatalf("dirty content changed: %q", data)
	}
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

func assertScopedNetworkCredential(t *testing.T, capturePath, canonicalURL string, credential GitCredential, networkSubcommands ...string) {
	t.Helper()
	encodedCredential := base64.StdEncoding.EncodeToString([]byte(credential.Username + ":" + credential.Token))
	seenNetwork := make(map[string]bool, len(networkSubcommands))
	for _, capture := range readCapturedProcesses(t, capturePath) {
		subcommand := capturedGitSubcommand(capture.Args)
		serialized := strings.Join(append(append([]string{}, capture.Args...), capture.Env...), "\n")
		if slices.Contains(networkSubcommands, subcommand) {
			seenNetwork[subcommand] = true
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
	for _, subcommand := range networkSubcommands {
		if !seenNetwork[subcommand] {
			t.Fatalf("network operation %s was not observed", subcommand)
		}
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
	installCapturedGitForPushTest(t, "https://github.com/owner/repo.git", bare)
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
