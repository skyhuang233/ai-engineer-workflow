package onboarding

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func PublishDefaultBranch(ctx context.Context, repository, remoteURL, branch string, credential GitCredential) error {
	if remoteURL == "" || branch == "" {
		return fmt.Errorf("publication remote and branch are required")
	}
	if _, err := gitOutput(ctx, repository, "remote", "get-url", "origin"); err != nil {
		if _, err := gitOutput(ctx, repository, "remote", "add", "origin", remoteURL); err != nil {
			return err
		}
	}
	command := exec.CommandContext(ctx, "git", "push", "-u", "origin", "refs/heads/"+branch+":refs/heads/"+branch)
	command.Dir = repository
	command.Env = append(os.Environ(), gitCredentialEnvironment(credential)...)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("publish default branch: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func SafeFastForward(ctx context.Context, repository, branch string, credential GitCredential) error {
	command := exec.CommandContext(ctx, "git", "fetch", "origin", "refs/heads/"+branch)
	command.Dir = repository
	command.Env = append(os.Environ(), gitCredentialEnvironment(credential)...)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("fetch merged default branch: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	command = exec.CommandContext(ctx, "git", "merge", "--ff-only", "FETCH_HEAD")
	command.Dir = repository
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("safe fast-forward merged default branch: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}
