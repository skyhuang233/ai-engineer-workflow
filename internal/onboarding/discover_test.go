package onboarding

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDiscoverPublishedRepositoryWithoutMutatingGitMetadata(t *testing.T) {
	remote := filepath.Join(t.TempDir(), "owner", "repo.git")
	if err := os.MkdirAll(filepath.Dir(remote), 0o700); err != nil {
		t.Fatal(err)
	}
	git(t, "", "init", "--bare", remote)
	repo := filepath.Join(t.TempDir(), "repo")
	git(t, "", "clone", remote, repo)
	git(t, repo, "config", "user.email", "test@example.com")
	git(t, repo, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "README.md")
	git(t, repo, "commit", "-m", "initial")
	git(t, repo, "branch", "-M", "main")
	git(t, repo, "push", "-u", "origin", "main")
	git(t, "", "--git-dir", remote, "symbolic-ref", "HEAD", "refs/heads/main")
	git(t, repo, "remote", "set-url", "origin", "https://github.com/owner/repo.git")
	headBefore := readOptional(t, filepath.Join(repo, ".git", "FETCH_HEAD"))
	result, err := Discover(context.Background(), repo, StaticRemoteHead{DefaultBranch: "main", Head: testGitOutput(t, repo, "rev-parse", "HEAD")})
	if err != nil {
		t.Fatal(err)
	}
	if result.Repository != "owner/repo" || result.DefaultBranch != "main" || !result.Published {
		t.Fatalf("result=%#v", result)
	}
	if after := readOptional(t, filepath.Join(repo, ".git", "FETCH_HEAD")); string(after) != string(headBefore) {
		t.Fatal("discovery mutated FETCH_HEAD")
	}
}

func TestDiscoverAllowsUnrelatedDirtyAndBlocksManagedDirty(t *testing.T) {
	repo := newRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "notes.txt"), []byte("dirty"), 0o600); err != nil {
		t.Fatal(err)
	}
	head := testGitOutput(t, repo, "rev-parse", "HEAD")
	result, err := Discover(context.Background(), repo, StaticRemoteHead{DefaultBranch: "main", Head: head})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Dirty {
		t.Fatal("dirty state not observed")
	}
	if err := os.MkdirAll(filepath.Join(repo, "docs", "agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "docs", "agents", "domain.md"), []byte("conflict"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Discover(context.Background(), repo, StaticRemoteHead{DefaultBranch: "main", Head: head}); err == nil {
		t.Fatal("managed path conflict accepted")
	}
}

func TestDiscoverBlocksRenameWhenEitherPathCrossesManagedBoundary(t *testing.T) {
	for _, test := range []struct {
		name, source, destination string
	}{
		{name: "into managed boundary", source: "notes.txt", destination: "docs/agents/domain.md"},
		{name: "out of managed boundary", source: "docs/agents/domain.md", destination: "notes.txt"},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := newRepo(t)
			source := filepath.Join(repo, filepath.FromSlash(test.source))
			if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(source, []byte("tracked\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			git(t, repo, "add", test.source)
			git(t, repo, "commit", "-m", "add rename source")
			destination := filepath.Join(repo, filepath.FromSlash(test.destination))
			if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
				t.Fatal(err)
			}
			git(t, repo, "mv", test.source, test.destination)
			head := testGitOutput(t, repo, "rev-parse", "HEAD")
			_, err := Discover(context.Background(), repo, StaticRemoteHead{DefaultBranch: "main", Head: head})
			if err == nil || !strings.Contains(err.Error(), "Workflow-managed path") {
				t.Fatalf("managed rename accepted: %v", err)
			}
		})
	}
}

func TestDiscoverStatusDoesNotRefreshOrLockGitIndex(t *testing.T) {
	repo := newRepo(t)
	index := filepath.Join(repo, ".git", "index")
	before, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	fixed := time.Unix(1_700_000_000, 0)
	if err := os.Chtimes(index, fixed, fixed); err != nil {
		t.Fatal(err)
	}
	readme := filepath.Join(repo, "README.md")
	if err := os.Chtimes(readme, fixed.Add(2*time.Hour), fixed.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	head := testGitOutput(t, repo, "rev-parse", "HEAD")
	if _, err := Discover(context.Background(), repo, StaticRemoteHead{DefaultBranch: "main", Head: head}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(index)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) || !info.ModTime().Equal(fixed) {
		t.Fatalf("read-only discovery refreshed index: bytes_changed=%t mtime=%s", !bytes.Equal(after, before), info.ModTime())
	}
	if _, err := os.Stat(index + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("read-only discovery left index.lock: %v", err)
	}
}

func TestDiscoverRejectsNonGitHubOriginWrongBranchAndHeadDrift(t *testing.T) {
	repo := newRepo(t)
	git(t, repo, "remote", "set-url", "origin", "https://gitlab.com/owner/repo.git")
	if _, err := Discover(context.Background(), repo, StaticRemoteHead{DefaultBranch: "main", Head: testGitOutput(t, repo, "rev-parse", "HEAD")}); err == nil {
		t.Fatal("non-GitHub origin accepted")
	}
	git(t, repo, "remote", "set-url", "origin", "git@github.com:owner/repo.git")
	git(t, repo, "checkout", "-b", "feature")
	if _, err := Discover(context.Background(), repo, StaticRemoteHead{DefaultBranch: "main", Head: testGitOutput(t, repo, "rev-parse", "HEAD")}); err == nil {
		t.Fatal("wrong branch accepted")
	}
	git(t, repo, "checkout", "main")
	if _, err := Discover(context.Background(), repo, StaticRemoteHead{DefaultBranch: "main", Head: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}); err == nil {
		t.Fatal("head drift accepted")
	}
}

func TestDiscoverRejectsRepositoryLocalInsteadOfOriginAlias(t *testing.T) {
	repo := newRepo(t)
	git(t, repo, "remote", "set-url", "origin", "workflow-alias:owner/repo.git")
	git(t, repo, "config", "url.https://github.com/.insteadOf", "workflow-alias:")
	called := false
	_, err := Discover(context.Background(), repo, remoteHeadFunc(func(context.Context, string) (string, string, error) {
		called = true
		return "main", testGitOutput(t, repo, "rev-parse", "HEAD"), nil
	}))
	if err == nil || called {
		t.Fatalf("repository-local insteadOf alias reached remote discovery: called=%t err=%v", called, err)
	}
}

func TestDiscoverRejectsAmbiguousRepositoryLocalOrigin(t *testing.T) {
	repo := newRepo(t)
	git(t, repo, "config", "--add", "remote.origin.url", "https://github.com/owner/other.git")
	called := false
	_, err := Discover(context.Background(), repo, remoteHeadFunc(func(context.Context, string) (string, string, error) {
		called = true
		return "main", testGitOutput(t, repo, "rev-parse", "HEAD"), nil
	}))
	if err == nil || called {
		t.Fatalf("ambiguous local origin was treated as absent: called=%t err=%v", called, err)
	}
}

func TestDiscoverRejectsExecutableLocalGitConfigurationWithoutExecutingIt(t *testing.T) {
	for _, key := range []string{"include.path", "includeIf.gitdir:/**.path", "core.fsmonitor", "filter.evil.process", "diff.evil.textconv", "merge.evil.driver", "credential.helper", "http.proxy"} {
		t.Run(key, func(t *testing.T) {
			repo := newRepo(t)
			sideEffect := filepath.Join(t.TempDir(), "executed")
			git(t, repo, "config", key, "malicious-command "+sideEffect)
			called := false
			_, err := Discover(context.Background(), repo, remoteHeadFunc(func(context.Context, string) (string, string, error) { called = true; return "", "", nil }))
			if err == nil || called {
				t.Fatalf("unsafe %s reached discovery: called=%t err=%v", key, called, err)
			}
			if _, statErr := os.Stat(sideEffect); !os.IsNotExist(statErr) {
				t.Fatalf("unsafe %s executed: %v", key, statErr)
			}
		})
	}
}

func TestDiscoverRejectsExecutableWorktreeGitConfigurationWithoutExecutingIt(t *testing.T) {
	main := newRepo(t)
	git(t, main, "config", "extensions.worktreeConfig", "true")
	worktree := filepath.Join(t.TempDir(), "linked-worktree")
	git(t, main, "worktree", "add", "-b", "linked", worktree)
	sideEffect := filepath.Join(t.TempDir(), "executed")
	included := filepath.Join(t.TempDir(), "included.gitconfig")
	if err := os.WriteFile(included, []byte("[core]\n\tfsmonitor = malicious-command "+sideEffect+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, worktree, "config", "--worktree", "include.path", included)
	called := false
	_, err := Discover(context.Background(), worktree, remoteHeadFunc(func(context.Context, string) (string, string, error) {
		called = true
		return "", "", nil
	}))
	if err == nil || called {
		t.Fatalf("unsafe worktree configuration reached discovery: called=%t err=%v", called, err)
	}
	if _, statErr := os.Stat(sideEffect); !os.IsNotExist(statErr) {
		t.Fatalf("unsafe worktree configuration executed: %v", statErr)
	}
}

func TestValidateLocalGitReadConfigurationRejectsWorktreeRedirectsInEveryRepositoryScope(t *testing.T) {
	for _, test := range []struct {
		name, scope, key, value string
	}{
		{name: "local core worktree", scope: "--local", key: "core.worktree", value: t.TempDir()},
		{name: "local alternate refs command", scope: "--local", key: "core.alternateRefsCommand", value: "malicious-command"},
		{name: "worktree core worktree", scope: "--worktree", key: "core.worktree", value: t.TempDir()},
		{name: "worktree alternate refs command", scope: "--worktree", key: "core.alternateRefsCommand", value: "malicious-command"},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := newRepo(t)
			if test.scope == "--worktree" {
				git(t, repo, "config", "--local", "extensions.worktreeConfig", "true")
			}
			git(t, repo, "config", test.scope, test.key, test.value)
			if err := ValidateLocalGitReadConfiguration(context.Background(), repo); err == nil || !strings.Contains(err.Error(), "unsafe repository-local Git configuration") {
				t.Fatalf("%s %s was accepted: %v", test.scope, test.key, err)
			}
		})
	}
}

func TestDiscoverRejectsCoreWorktreeBeforeRevParseCanEscapeRepository(t *testing.T) {
	repo := newRepo(t)
	externalWorktree := t.TempDir()
	git(t, repo, "config", "--local", "core.worktree", externalWorktree)

	unsafe := exec.Command("git", "-C", repo, "rev-parse", "--show-toplevel")
	unsafeOutput, err := unsafe.Output()
	if err != nil {
		t.Fatalf("prove hostile core.worktree redirect: %v", err)
	}
	unsafeRoot := strings.TrimSpace(string(unsafeOutput))
	externalRoot, err := filepath.Abs(externalWorktree)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(filepath.Clean(unsafeRoot), filepath.Clean(externalRoot)) {
		t.Fatalf("test precondition did not redirect rev-parse: got %q want %q", unsafeRoot, externalRoot)
	}

	discovery, err := Discover(context.Background(), repo, nil)
	if err == nil || !strings.Contains(err.Error(), "unsafe repository-local Git configuration") {
		t.Fatalf("Discover escaped to %#v instead of rejecting core.worktree: %v", discovery, err)
	}
}

func TestReadLocalOriginURLRejectsLeadingOrTrailingWhitespace(t *testing.T) {
	for _, origin := range []string{
		" https://github.com/owner/repo.git",
		"https://github.com/owner/repo.git ",
		"\thttps://github.com/owner/repo.git",
		"\nhttps://github.com/owner/repo.git",
		"https://github.com/owner/repo.git\n",
	} {
		t.Run(fmt.Sprintf("%q", origin), func(t *testing.T) {
			repo := newRepo(t)
			git(t, repo, "config", "--local", "remote.origin.url", origin)
			if got, err := ReadLocalOriginURL(context.Background(), repo); err == nil {
				t.Fatalf("whitespace-bearing raw origin was normalized to %q", got)
			}
		})
	}
}

func TestGitHubOriginAcceptsOnlyCanonicalTransportForms(t *testing.T) {
	for _, accepted := range []string{
		"https://github.com/owner/repo",
		"https://github.com/owner/repo.git",
		"git@github.com:owner/repo",
		"git@github.com:owner/repo.git",
	} {
		if got, err := ParseGitHubOrigin(accepted); err != nil || got != "owner/repo" {
			t.Errorf("canonical origin %q parsed as %q, %v", accepted, got, err)
		}
	}
	for _, rejected := range []string{
		"http://github.com/owner/repo.git",
		"https://user@github.com/owner/repo.git",
		"https://github.com:443/owner/repo.git",
		"https://github.com/owner/repo.git?token=secret",
		"https://github.com/owner/repo.git#fragment",
		"ssh://git@github.com/owner/repo.git",
		"other@github.com:owner/repo.git",
		"git@github.com:22/owner/repo.git",
		"git@github.com:owner/repo/extra.git",
		"git@github.com:owner/repo?token=x",
		"git@github.com:owner/repo#fragment",
		"git@github.com:owner/repo:alternate",
		"git@github.com:owner/repo@attacker",
		"git@github.com:owner/repo\\alternate",
		"git@github.com:owner/re%70o",
		"git@github.com:owner/..",
		"git@github.com:./repo",
		"git@github.com:owner/repo name",
	} {
		if got, err := ParseGitHubOrigin(rejected); err == nil {
			t.Errorf("noncanonical origin %q accepted as %q", rejected, got)
		}
	}
}

func newRepo(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	git(t, "", "init", "-b", "main", repo)
	git(t, repo, "config", "user.email", "test@example.com")
	git(t, repo, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "README.md")
	git(t, repo, "commit", "-m", "initial")
	git(t, repo, "remote", "add", "origin", "https://github.com/owner/repo.git")
	return repo
}
func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	if dir != "" {
		command.Dir = dir
	}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}
func testGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	for len(output) > 0 && (output[len(output)-1] == '\n' || output[len(output)-1] == '\r') {
		output = output[:len(output)-1]
	}
	return string(output)
}
func readOptional(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return data
}
