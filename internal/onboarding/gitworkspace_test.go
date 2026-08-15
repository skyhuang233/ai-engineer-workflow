package onboarding

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareOnboardingBranchUsesTemporaryCloneOnly(t *testing.T) {
	source := newRepo(t)
	bare := filepath.Join(t.TempDir(), "remote.git")
	git(t, "", "clone", "--bare", source, bare)
	base := testGitOutput(t, source, "rev-parse", "HEAD")
	dirty := filepath.Join(source, "local.txt")
	if err := os.WriteFile(dirty, []byte("user owned"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace, err := PrepareOnboardingBranch(context.Background(), "owner/repo", bare, base, t.TempDir(), repeatString("a", 64), map[string][]byte{"managed.txt": []byte("contract\n")}, GitCredential{})
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Cleanup()
	if workspace.Head == base || workspace.Branch != "workflow/onboarding-aaaaaaaaaaaa" {
		t.Fatalf("workspace=%#v", workspace)
	}
	if data, err := os.ReadFile(dirty); err != nil || string(data) != "user owned" {
		t.Fatal("source working copy changed")
	}
	remoteHead := testGitOutput(t, "", "--git-dir", bare, "rev-parse", "refs/heads/"+workspace.Branch)
	if remoteHead != workspace.Head {
		t.Fatalf("remote=%s head=%s", remoteHead, workspace.Head)
	}
}

func TestPrepareOnboardingBranchPreservesCloneAndCleanupFailures(t *testing.T) {
	cleanupCalled := false
	_, err := prepareOnboardingBranch(
		context.Background(),
		"owner/repo",
		filepath.Join(t.TempDir(), "missing.git"),
		repeatString("b", 40),
		t.TempDir(),
		repeatString("a", 64),
		map[string][]byte{"managed.txt": []byte("contract\n")},
		GitCredential{},
		func(string) error {
			cleanupCalled = true
			return errors.New("temporary workspace removal denied")
		},
	)
	if err == nil {
		t.Fatal("clone preparation reported success")
	}
	for _, wanted := range []string{"git clone", "temporary workspace removal denied"} {
		if !strings.Contains(err.Error(), wanted) {
			t.Fatalf("combined error %q lacks %q", err, wanted)
		}
	}
	if !cleanupCalled {
		t.Fatal("temporary workspace cleanup was not attempted")
	}
}

func repeatString(value string, count int) string {
	result := ""
	for i := 0; i < count; i++ {
		result += value
	}
	return result
}
