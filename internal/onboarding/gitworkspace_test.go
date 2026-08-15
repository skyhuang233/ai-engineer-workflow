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

func TestPrepareOnboardingBranchConvergesToDeterministicCommitAndRejectsThirdState(t *testing.T) {
	source := newRepo(t)
	globalHooks := filepath.Join(t.TempDir(), "global-hooks")
	if err := os.Mkdir(globalHooks, 0o700); err != nil {
		t.Fatal(err)
	}
	hookSideEffect := filepath.Join(t.TempDir(), "unapproved-hook-ran")
	if err := os.WriteFile(filepath.Join(globalHooks, "pre-commit"), []byte("#!/bin/sh\nprintf invoked > \"$WORKFLOW_TEST_HOOK_SIDE_EFFECT\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	globalConfig := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(globalConfig, []byte("[core]\n\thooksPath = "+strings.ReplaceAll(globalHooks, `\`, `/`)+"\n[commit]\n\tgpgSign = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalConfig)
	t.Setenv("WORKFLOW_TEST_HOOK_SIDE_EFFECT", hookSideEffect)

	base := testGitOutput(t, source, "rev-parse", "HEAD")
	bare := filepath.Join(t.TempDir(), "remote.git")
	git(t, "", "clone", "--bare", source, bare)
	digest := repeatString("a", 64)
	files := map[string][]byte{"managed.txt": []byte("contract\n")}
	first, err := PrepareOnboardingBranch(context.Background(), "owner/repo", bare, base, t.TempDir(), digest, files, GitCredential{})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Cleanup()
	second, err := PrepareOnboardingBranch(context.Background(), "owner/repo", bare, base, t.TempDir(), digest, files, GitCredential{})
	if err != nil {
		t.Fatalf("same approved branch retry did not converge: %v", err)
	}
	defer second.Cleanup()
	if first.Head != second.Head {
		t.Fatalf("same plan/source/tree produced %s then %s", first.Head, second.Head)
	}
	if _, err := os.Stat(hookSideEffect); !os.IsNotExist(err) {
		t.Fatalf("onboarding commit executed an unapproved user Git hook: %v", err)
	}

	attacker := filepath.Join(t.TempDir(), "third-state")
	git(t, "", "clone", "--branch", first.Branch, bare, attacker)
	git(t, attacker, "config", "user.name", "Other")
	git(t, attacker, "config", "user.email", "other@example.com")
	if err := os.WriteFile(filepath.Join(attacker, "unapproved.txt"), []byte("third state\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, attacker, "add", "unapproved.txt")
	git(t, attacker, "commit", "-m", "unapproved")
	git(t, attacker, "push", "origin", "HEAD:"+first.Branch)
	if _, err := PrepareOnboardingBranch(context.Background(), "owner/repo", bare, base, t.TempDir(), digest, files, GitCredential{}); err == nil || !strings.Contains(err.Error(), "push") {
		t.Fatalf("external onboarding branch third state was accepted: %v", err)
	}
}

func repeatString(value string, count int) string {
	result := ""
	for i := 0; i < count; i++ {
		result += value
	}
	return result
}
