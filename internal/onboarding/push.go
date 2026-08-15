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
	if err := ValidateAuthenticatedGitRepository(ctx, repository); err != nil {
		return err
	}
	origin, originErr := rawOriginURL(ctx, repository)
	if errors.Is(originErr, errRepositoryOriginAbsent) {
		command := exec.CommandContext(ctx, "git", hardenedLocalGitArgs("remote", "add", "origin", canonicalURL)...)
		command.Dir = repository
		command.Env = isolatedGitEnvironment(nil)
		if output, err := command.CombinedOutput(); err != nil {
			return fmt.Errorf("add canonical origin: %w (%s)", err, strings.TrimSpace(string(output)))
		}
		if _, err := rawOriginURL(ctx, repository); err != nil {
			return err
		}
	} else if originErr != nil {
		return originErr
	} else {
		originRepository, parseErr := parseGitHubOrigin(origin)
		if parseErr != nil || !strings.EqualFold(originRepository, repositoryID) {
			return errors.New("origin differs from the approved GitHub repository")
		}
	}
	args := hardenedAuthenticatedGitArgs(canonicalURL, "push", canonicalURL, "refs/heads/"+branch+":refs/heads/"+branch)
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = repository
	command.Env = isolatedGitEnvironment(gitCredentialEnvironmentForURL(credential, canonicalURL))
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("publish default branch: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func SafeFastForward(ctx context.Context, repository, repositoryID, branch, expectedPreMergeHead, expectedMergeHead string, credential GitCredential) error {
	if err := ValidateAuthenticatedGitRepository(ctx, repository); err != nil {
		return err
	}
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
	status, err := gitBytes(ctx, repository, "status", "--porcelain=v2", "-z", "--untracked-files=all")
	if err != nil {
		return err
	}
	if len(status) != 0 {
		return errors.New("safe fast-forward requires a clean working tree")
	}
	remoteURL, err := GitHubHTTPSURL(repositoryID)
	if err != nil {
		return err
	}
	command := exec.CommandContext(ctx, "git", hardenedAuthenticatedGitArgs(remoteURL, "fetch", remoteURL, expectedMergeHead)...)
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
	status, err = gitBytes(ctx, repository, "status", "--porcelain=v2", "-z", "--untracked-files=all")
	if err != nil || len(status) != 0 {
		return errors.Join(errors.New("working tree changed before safe fast-forward"), err)
	}
	command = exec.CommandContext(ctx, "git", hardenedLocalGitArgs("merge", "--ff-only", "--no-autostash", "FETCH_HEAD")...)
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

func hardenedAuthenticatedGitArgs(canonicalURL string, args ...string) []string {
	prefix := hardenedLocalGitArgs()
	prefix = append(prefix,
		"-c", "http.sslVerify=true",
		"-c", "http."+canonicalURL+".sslVerify=true",
		"-c", "protocol.allow=never",
		"-c", "protocol.https.allow=always",
	)
	return append(prefix, args...)
}

func hardenedLocalGitArgs(args ...string) []string {
	prefix := []string{
		"-c", "core.hooksPath=" + os.DevNull,
		"-c", "credential.helper=",
		"-c", "credential.interactive=never",
		"-c", "core.fsmonitor=false",
		"-c", "commit.gpgSign=false",
		"-c", "tag.gpgSign=false",
		"-c", "merge.autoStash=false",
		"-c", "merge.verifySignatures=false",
	}
	return append(prefix, args...)
}
