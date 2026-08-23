package agent

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

func TestValidateLocalRemotesAcceptsContainerAbsoluteGateAndRejectsNetworkURL(t *testing.T) {
	dir := t.TempDir()
	origin := t.TempDir()
	gitTestCommand(t, dir, "init")
	gitTestCommand(t, dir, "remote", "add", "origin", origin)
	gitTestCommand(t, dir, "remote", "add", "no-mistakes", "/codex-state/no-mistakes/repos/ticket.git")
	if err := validateLocalRemotes(context.Background(), dir); err != nil {
		t.Fatalf("container-local gate remote rejected: %v", err)
	}
	gitTestCommand(t, dir, "remote", "set-url", "no-mistakes", "https://user:token@github.com/owner/repo.git")
	if err := validateLocalRemotes(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "absolute local path") {
		t.Fatalf("network remote error = %v", err)
	}
}

func gitTestCommand(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v (%s)", strings.Join(args, " "), err, output)
	}
}
