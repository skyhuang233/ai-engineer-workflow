package github

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestFetchDeliverySourceUpdatesHeadWithoutPersistingRemote(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	runSourceFetcherGit(t, "", "init", "--bare", remote)
	publisher := filepath.Join(root, "publisher")
	runSourceFetcherGit(t, "", "init", "-b", "main", publisher)
	runSourceFetcherGit(t, publisher, "config", "user.name", "Test")
	runSourceFetcherGit(t, publisher, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(publisher, "source.txt"), []byte("advanced\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runSourceFetcherGit(t, publisher, "add", "source.txt")
	runSourceFetcherGit(t, publisher, "commit", "-m", "advanced")
	runSourceFetcherGit(t, publisher, "remote", "add", "origin", remote)
	runSourceFetcherGit(t, publisher, "push", "origin", "main")
	snapshot := filepath.Join(root, "snapshot.git")
	runSourceFetcherGit(t, "", "init", "--bare", snapshot)
	if err := fetchDeliverySource(ctx, snapshot, remote, "refs/heads/main", nil); err != nil {
		t.Fatal(err)
	}
	want := sourceFetcherGitOutput(t, publisher, "rev-parse", "refs/heads/main")
	got := sourceFetcherGitOutput(t, snapshot, "rev-parse", "refs/heads/main")
	if got != want {
		t.Fatalf("fetched Delivery Source head = %q, want %q", got, want)
	}
	if remotes := sourceFetcherGitOutput(t, snapshot, "remote"); remotes != "" {
		t.Fatalf("Delivery Source persisted remote configuration %q", remotes)
	}
}

func runSourceFetcherGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

func sourceFetcherGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(output))
}
