package onboarding

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func PublishDefaultBranch(ctx context.Context, repository, remoteURL, branch string, credential GitCredential) error {
	if remoteURL == "" || branch == "" {
		return fmt.Errorf("publication remote and branch are required")
	}
	repositoryID, err := parseGitHubOrigin(remoteURL)
	if err != nil {
		return err
	}
	canonicalURL, err := GitHubHTTPSURL(repositoryID)
	if err != nil {
		return err
	}
	pushTarget := canonicalURL
	origin, originErr := gitOutput(ctx, repository, "remote", "get-url", "origin")
	if originErr != nil {
		if _, err := gitOutput(ctx, repository, "remote", "add", "origin", canonicalURL); err != nil {
			return err
		}
		pushTarget = "origin"
	} else {
		originRepository, parseErr := parseGitHubOrigin(origin)
		if parseErr != nil || !strings.EqualFold(originRepository, repositoryID) {
			return errors.New("origin differs from the approved GitHub repository")
		}
	}
	args := []string{"push"}
	if pushTarget == "origin" {
		args = append(args, "-u")
	}
	args = append(args, pushTarget, "refs/heads/"+branch+":refs/heads/"+branch)
	args = hardenedAuthenticatedGitArgs(args...)
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = repository
	command.Env = isolatedGitEnvironment(gitCredentialEnvironmentForURL(credential, canonicalURL))
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("publish default branch: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func SafeFastForward(ctx context.Context, repository, repositoryID, branch, expectedPreMergeHead, expectedMergeHead string, credential GitCredential) error {
	currentBranch, err := gitOutput(ctx, repository, "branch", "--show-current")
	if err != nil || currentBranch != branch {
		return fmt.Errorf("safe fast-forward requires checked-out branch %q", branch)
	}
	currentHead, err := gitOutput(ctx, repository, "rev-parse", "--verify", "HEAD")
	if err != nil || !fullSHA.MatchString(expectedPreMergeHead) || currentHead != expectedPreMergeHead {
		return errors.New("local pre-merge HEAD differs from the approved onboarding base")
	}
	if !fullSHA.MatchString(expectedMergeHead) {
		return errors.New("persisted onboarding merge HEAD is invalid")
	}
	remoteURL, err := GitHubHTTPSURL(repositoryID)
	if err != nil {
		return err
	}
	command := exec.CommandContext(ctx, "git", hardenedAuthenticatedGitArgs("fetch", remoteURL, expectedMergeHead)...)
	command.Dir = repository
	command.Env = isolatedGitEnvironment(gitCredentialEnvironmentForURL(credential, remoteURL))
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("fetch merged default branch: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	fetchedHead, err := gitOutput(ctx, repository, "rev-parse", "--verify", "FETCH_HEAD")
	if err != nil || fetchedHead != expectedMergeHead {
		return errors.New("fetched revision differs from the persisted onboarding merge HEAD")
	}
	currentBranch, err = gitOutput(ctx, repository, "branch", "--show-current")
	if err != nil || currentBranch != branch {
		return fmt.Errorf("checked-out branch changed before safe fast-forward of %q", branch)
	}
	currentHead, err = gitOutput(ctx, repository, "rev-parse", "--verify", "HEAD")
	if err != nil || currentHead != expectedPreMergeHead {
		return errors.New("local HEAD changed before safe fast-forward")
	}
	command = exec.CommandContext(ctx, "git", "merge", "--ff-only", "FETCH_HEAD")
	command.Dir = repository
	command.Env = isolatedGitEnvironment(nil)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("safe fast-forward merged default branch: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	mergedHead, err := gitOutput(ctx, repository, "rev-parse", "--verify", "HEAD")
	if err != nil || mergedHead != expectedMergeHead {
		return errors.New("local default branch did not reach the persisted onboarding merge HEAD")
	}
	return nil
}

func hardenedAuthenticatedGitArgs(args ...string) []string {
	prefix := []string{
		"-c", "core.hooksPath=" + os.DevNull,
		"-c", "credential.helper=",
		"-c", "credential.interactive=never",
		"-c", "core.fsmonitor=false",
	}
	return append(prefix, args...)
}
