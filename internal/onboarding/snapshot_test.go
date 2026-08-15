package onboarding

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApprovalSnapshotAcceptsOnlyEvidenceBoundInitialBaselineTransition(t *testing.T) {
	ctx := context.Background()
	repo := filepath.Join(t.TempDir(), "repo")
	git(t, "", "init", "-b", "main", repo)
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("approved\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	discovery, err := Discover(ctx, repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := BaselineSnapshot(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := CaptureApprovalSnapshot(ctx, discovery, "owner/repo", baseline)
	if err != nil {
		t.Fatal(err)
	}
	head, err := CreateInitialBaseline(ctx, repo, "main", baseline, "Initial Repository Baseline\n\nSetup-Plan-SHA256: "+strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyApprovalSnapshotTransitions(ctx, snapshot, ApprovalTransitions{}); err == nil {
		t.Fatal("matching baseline state was accepted without this plan's durable effect evidence")
	}
	if err := VerifyApprovalSnapshotTransitions(ctx, snapshot, ApprovalTransitions{InitialBaselineHead: strings.Repeat("b", 40)}); err == nil {
		t.Fatal("baseline state was accepted with evidence for a different SHA")
	}
	if err := VerifyApprovalSnapshotTransitions(ctx, snapshot, ApprovalTransitions{InitialBaselineHead: head}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "unrelated.txt"), []byte("third state\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyApprovalSnapshotTransitions(ctx, snapshot, ApprovalTransitions{InitialBaselineHead: head}); err == nil {
		t.Fatalf("third dirty state was accepted: %v", err)
	}
}

func TestApprovalSnapshotBindsPublishedOriginAndMergeToExactEffectEvidence(t *testing.T) {
	ctx := context.Background()
	repo := filepath.Join(t.TempDir(), "repo")
	git(t, "", "init", "-b", "main", repo)
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "README.md")
	git(t, repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "base")
	discovery, err := Discover(ctx, repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := CaptureApprovalSnapshot(ctx, discovery, "owner/repo", nil)
	if err != nil {
		t.Fatal(err)
	}
	git(t, repo, "remote", "add", "origin", "https://github.com/owner/repo.git")
	if err := VerifyApprovalSnapshotTransitions(ctx, snapshot, ApprovalTransitions{CreatedRepository: "owner/repo"}); err == nil {
		t.Fatal("published origin was accepted without publish effect evidence")
	}
	if err := VerifyApprovalSnapshotTransitions(ctx, snapshot, ApprovalTransitions{CreatedRepository: "owner/repo", PublishedHistoryHead: discovery.Head}); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(repo, "unrelated.txt"), []byte("preserve\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Capture the published-dirty state that local_fast_forward must preserve.
	published, err := Discover(ctx, repo, StaticRemoteHead{DefaultBranch: "main", Head: discovery.Head})
	if err != nil {
		t.Fatal(err)
	}
	publishedSnapshot, err := CaptureApprovalSnapshot(ctx, published, "owner/repo", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".workflow"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".workflow", "repository.json"), []byte("approved contract\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", ".workflow/repository.json")
	git(t, repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "approved merge")
	mergeHead := testGitOutput(t, repo, "rev-parse", "HEAD")
	if err := VerifyApprovalSnapshotTransitions(ctx, publishedSnapshot, ApprovalTransitions{MergedHead: strings.Repeat("c", 40)}); err == nil {
		t.Fatal("different merge SHA evidence was accepted")
	}
	if err := VerifyApprovalSnapshotTransitions(ctx, publishedSnapshot, ApprovalTransitions{MergedHead: mergeHead}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(repo, "unrelated.txt"))
	if err != nil || string(data) != "preserve\n" {
		t.Fatalf("unrelated dirty file changed: %q %v", data, err)
	}
}
