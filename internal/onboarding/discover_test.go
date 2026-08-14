package onboarding

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
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
