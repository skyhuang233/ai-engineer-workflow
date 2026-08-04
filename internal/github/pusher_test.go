package github

import (
	"context"
	"strings"
	"testing"
)

func TestWorkspacePusherRejectsPushURLWithEmbeddedCredential(t *testing.T) {
	err := (WorkspacePusher{WorkspacePath: t.TempDir(), Token: "secret-token", PushURL: "https://x-access-token:secret-token@github.com/owner/repo.git"}).Push(context.Background(), "owner/repo", "ticket-1", "abc123", "", true)
	if err == nil || !strings.Contains(err.Error(), "without embedded credentials") {
		t.Fatalf("embedded credential error = %v", err)
	}
}
