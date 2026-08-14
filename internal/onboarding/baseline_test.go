package onboarding

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestInitialBaselineUsesTemporaryIndexAndExactApprovedFiles(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	git(t, "", "init", "-b", "main", repo)
	git(t, repo, "config", "user.email", "test@example.com")
	git(t, repo, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "staged.txt"), []byte("staged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "staged.txt")
	if err := os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("untracked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "ignored.txt"), []byte("ignored\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("ignored.txt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(repo, ".git", "index")
	before := readOptional(t, indexPath)
	files, err := BaselineFiles(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{".gitignore", "staged.txt", "untracked.txt"}
	if !reflect.DeepEqual(files, want) {
		t.Fatalf("files=%v want=%v", files, want)
	}
	commit, err := CreateInitialBaseline(context.Background(), repo, "main", files, "Initial repository baseline")
	if err != nil {
		t.Fatal(err)
	}
	if len(commit) != 40 {
		t.Fatalf("commit=%q", commit)
	}
	after := readOptional(t, indexPath)
	if string(before) != string(after) {
		t.Fatal("user index bytes changed")
	}
	tree := testGitOutput(t, repo, "ls-tree", "-r", "--name-only", "HEAD")
	if tree != ".gitignore\nstaged.txt\nuntracked.txt" {
		t.Fatalf("tree=%q", tree)
	}
}
func TestInitialBaselineRejectsCredentialMaterialAndPlanDrift(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	git(t, "", "init", "-b", "main", repo)
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("GITHUB_TOKEN=ghp_abcdefghijklmnopqrstuvwxyz1234567890\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	files, err := BaselineFiles(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if findings := ScanCredentialMaterial(repo, files); len(findings) == 0 {
		t.Fatal("credential material not detected")
	}
	if _, err := CreateInitialBaseline(context.Background(), repo, "main", []string{"different.txt"}, "baseline"); err == nil {
		t.Fatal("approved file drift accepted")
	}
}
