package onboarding

import (
	"context"
	"os"
	"path/filepath"
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
	workspace, err := PrepareOnboardingBranch(context.Background(), bare, base, t.TempDir(), repeatString("a", 64), map[string][]byte{"managed.txt": []byte("contract\n")}, GitCredential{})
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
func repeatString(value string, count int) string {
	result := ""
	for i := 0; i < count; i++ {
		result += value
	}
	return result
}
