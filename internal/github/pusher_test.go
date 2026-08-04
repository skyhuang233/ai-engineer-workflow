package github

import (
	"context"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
)

func TestGatewayLeaseTrackingRefPreservesNonBranchLocalPrefix(t *testing.T) {
	localRef := plumbing.ReferenceName("refs/workflow-gateway/candidate")
	got := gatewayLeaseTrackingRef(localRef)
	want := plumbing.ReferenceName("refs/remotes/workflow-gateway/refs/workflow-gateway/candidate")
	if got != want {
		t.Fatalf("tracking ref = %q, want %q", got, want)
	}
}

func TestWorkspacePusherRejectsPushURLWithEmbeddedCredential(t *testing.T) {
	err := (WorkspacePusher{WorkspacePath: t.TempDir(), Token: "secret-token", PushURL: "https://x-access-token:secret-token@github.com/owner/repo.git"}).Push(context.Background(), "owner/repo", "ticket-1", "abc123", "", true)
	if err == nil || !strings.Contains(err.Error(), "without embedded credentials") {
		t.Fatalf("embedded credential error = %v", err)
	}
}

func TestWorkspacePusherRejectsUnadmittedPushURL(t *testing.T) {
	err := (WorkspacePusher{WorkspacePath: t.TempDir(), Token: "secret-token", PushURL: "https://attacker.example/owner/repo.git"}).Push(context.Background(), "owner/repo", "ticket-1", "abc123", "", true)
	if err == nil || !strings.Contains(err.Error(), "admitted GitHub repository") {
		t.Fatalf("unadmitted URL error = %v", err)
	}
}
