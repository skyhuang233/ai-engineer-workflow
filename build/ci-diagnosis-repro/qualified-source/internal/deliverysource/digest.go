package deliverysource

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os/exec"
	"strings"
)

func Digest(ctx context.Context, sourcePath string) (string, error) {
	head, err := gitOutput(ctx, sourcePath, "symbolic-ref", "HEAD")
	if err != nil {
		return "", fmt.Errorf("read Delivery Source HEAD: %w", err)
	}
	identity, err := gitOutput(ctx, sourcePath, "config", "--local", "--get", "workflow.sourceIdentity")
	if err != nil {
		return "", fmt.Errorf("read Delivery Source identity: %w", err)
	}
	refs, err := gitOutput(ctx, sourcePath, "for-each-ref", "--sort=refname", "--format=%(refname) %(objectname)", "refs/heads", "refs/tags")
	if err != nil {
		return "", fmt.Errorf("read Delivery Source refs: %w", err)
	}
	digest := sha256.Sum256([]byte(strings.TrimSpace(head) + "\n" + strings.TrimSpace(identity) + "\n" + strings.TrimSpace(refs)))
	return fmt.Sprintf("%x", digest), nil
}

func gitOutput(ctx context.Context, sourcePath string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = sourcePath
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %v: %w", args, err)
	}
	return string(output), nil
}
