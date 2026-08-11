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
	runSourceFetcherGit(t, publisher, "branch", "stale")
	runSourceFetcherGit(t, publisher, "tag", "stale")
	runSourceFetcherGit(t, publisher, "remote", "add", "origin", remote)
	runSourceFetcherGit(t, publisher, "push", "origin", "main")
	snapshot := filepath.Join(root, "snapshot.git")
	runSourceFetcherGit(t, "", "init", "--bare", snapshot)
	runSourceFetcherGit(t, snapshot, "fetch", publisher, "+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*")
	if err := fetchDeliverySource(ctx, snapshot, remote, "refs/heads/main", nil); err != nil {
		t.Fatal(err)
	}
	want := sourceFetcherGitOutput(t, publisher, "rev-parse", "refs/heads/main")
	got := sourceFetcherGitOutput(t, snapshot, "rev-parse", "refs/heads/main")
	if got != want {
		t.Fatalf("fetched Delivery Source head = %q, want %q", got, want)
	}
	if refs := sourceFetcherGitOutput(t, snapshot, "for-each-ref", "--format=%(refname)", "refs/heads", "refs/tags"); refs != "refs/heads/main" {
		t.Fatalf("fetched Delivery Source refs = %q, want authoritative remote refs", refs)
	}
	if remotes := sourceFetcherGitOutput(t, snapshot, "remote"); remotes != "" {
		t.Fatalf("Delivery Source persisted remote configuration %q", remotes)
	}
}

func TestDeliverySourceRemoteURLUsesAdmittedAPIOrigin(t *testing.T) {
	tests := []struct {
		name    string
		apiBase string
		want    string
		wantErr bool
	}{
		{name: "public GitHub", apiBase: "https://api.github.com", want: "https://github.com/owner/repo.git"},
		{name: "GitHub Enterprise", apiBase: "https://github.example.com/api/v3", want: "https://github.example.com/owner/repo.git"},
		{name: "GitHub Enterprise port", apiBase: "https://github.example.com:8443/api/v3", want: "https://github.example.com:8443/owner/repo.git"},
		{name: "insecure origin", apiBase: "http://github.example.com/api/v3", wantErr: true},
		{name: "embedded credentials", apiBase: "https://token@github.example.com/api/v3", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := deliverySourceRemoteURL(test.apiBase, "owner/repo")
			if (err != nil) != test.wantErr || got != test.want {
				t.Fatalf("deliverySourceRemoteURL() = %q, %v; want %q, error=%t", got, err, test.want, test.wantErr)
			}
		})
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
