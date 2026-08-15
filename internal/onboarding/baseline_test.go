package onboarding

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
	snapshot, err := BaselineSnapshot(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	commit, err := CreateInitialBaseline(context.Background(), repo, "main", snapshot, "Initial repository baseline")
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
	if author := testGitOutput(t, repo, "show", "-s", "--format=%an <%ae>", "HEAD"); author != "Agent Workflow Setup <workflow@localhost>" {
		t.Fatalf("baseline author = %q", author)
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
	if _, err := CreateInitialBaseline(context.Background(), repo, "main", []BaselineFile{{Path: "different.txt", SHA256: strings.Repeat("a", 64)}}, "baseline"); err == nil {
		t.Fatal("approved file drift accepted")
	}
}

func TestInitialBaselineBindsAndRechecksExactFileBytes(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	git(t, "", "init", "-b", "main", repo)
	path := filepath.Join(repo, "README.md")
	approvedBytes := []byte("approved\n")
	if err := os.WriteFile(path, approvedBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := BaselineSnapshot(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(approvedBytes)
	if len(snapshot) != 1 || snapshot[0].Path != "README.md" || snapshot[0].SHA256 != hex.EncodeToString(want[:]) {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if err := os.WriteFile(path, []byte("replacement\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateInitialBaseline(context.Background(), repo, "main", snapshot, "baseline"); err == nil || !strings.Contains(err.Error(), "content drifted") {
		t.Fatalf("replacement bytes accepted: %v", err)
	}
}

func TestInitialBaselineBindsGitModeFromSnapshotThroughApply(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	git(t, "", "init", "-b", "main", repo)
	path := filepath.Join(repo, "script.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	blob := testGitOutput(t, repo, "hash-object", "-w", "script.sh")
	git(t, repo, "update-index", "--add", "--cacheinfo", "100755,"+blob+",script.sh")
	snapshot, err := BaselineSnapshot(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot) != 1 || snapshot[0].Mode != "100755" {
		t.Fatalf("baseline mode snapshot = %#v", snapshot)
	}
	git(t, repo, "update-index", "--cacheinfo", "100644,"+blob+",script.sh")
	if _, err := CreateInitialBaseline(context.Background(), repo, "main", snapshot, "baseline"); err == nil || !strings.Contains(err.Error(), "mode drifted") {
		t.Fatalf("git mode replacement accepted: %v", err)
	}
	git(t, repo, "update-index", "--cacheinfo", "100755,"+blob+",script.sh")
	commit, err := CreateInitialBaseline(context.Background(), repo, "main", snapshot, "baseline")
	if err != nil {
		t.Fatal(err)
	}
	if mode := strings.Fields(testGitOutput(t, repo, "ls-tree", commit, "--", "script.sh"))[0]; mode != "100755" {
		t.Fatalf("baseline tree mode = %q", mode)
	}
}

func TestInitialBaselineBindsSymlinkModeAndTargetBlob(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	git(t, "", "init", "-b", "main", repo)
	path := filepath.Join(repo, "current")
	if err := os.WriteFile(path, []byte("target-a"), 0o600); err != nil {
		t.Fatal(err)
	}
	blob := testGitOutput(t, repo, "hash-object", "-w", "current")
	git(t, repo, "update-index", "--add", "--cacheinfo", "120000,"+blob+",current")
	snapshot, err := BaselineSnapshot(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot) != 1 || snapshot[0].Mode != "120000" {
		t.Fatalf("symlink snapshot = %#v", snapshot)
	}
	if err := os.WriteFile(path, []byte("target-b"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateInitialBaseline(context.Background(), repo, "main", snapshot, "baseline"); err == nil || !strings.Contains(err.Error(), "content drifted") {
		t.Fatalf("symlink target replacement accepted: %v", err)
	}
	if err := os.WriteFile(path, []byte("target-a"), 0o600); err != nil {
		t.Fatal(err)
	}
	commit, err := CreateInitialBaseline(context.Background(), repo, "main", snapshot, "baseline")
	if err != nil {
		t.Fatal(err)
	}
	if entry := testGitOutput(t, repo, "ls-tree", commit, "--", "current"); !strings.HasPrefix(entry, "120000 ") {
		t.Fatalf("symlink tree entry = %q", entry)
	}
	if target := testGitOutput(t, repo, "show", commit+":current"); target != "target-a" {
		t.Fatalf("symlink blob = %q", target)
	}
}
