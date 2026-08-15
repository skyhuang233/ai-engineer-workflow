package onboarding

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

var errUnsafeRepositoryGitConfig = errors.New("unsafe repository-local Git configuration")
var errRepositoryOriginAbsent = errors.New("repository has no local origin URL")

// ValidateAuthenticatedGitRepository rejects repository-owned configuration
// that could redirect or weaken a later PAT-bearing Git transport operation.
func ValidateAuthenticatedGitRepository(ctx context.Context, repository string) error {
	command := exec.CommandContext(ctx, "git", "config", "--local", "--null", "--name-only", "--list", "--no-includes")
	command.Dir = repository
	command.Env = isolatedGitEnvironment(nil)
	output, err := command.Output()
	if err != nil {
		return fmt.Errorf("inspect repository-local Git configuration: %w", err)
	}
	for _, raw := range strings.Split(string(output), "\x00") {
		key := strings.ToLower(strings.TrimSpace(raw))
		if key == "" {
			continue
		}
		if unsafeRepositoryGitKey(key) {
			return fmt.Errorf("%w: %q is unsafe for authenticated transport", errUnsafeRepositoryGitConfig, raw)
		}
	}
	return nil
}

func unsafeRepositoryGitKey(key string) bool {
	return strings.HasPrefix(key, "http.") ||
		strings.HasPrefix(key, "https.") ||
		strings.HasPrefix(key, "credential.") ||
		strings.HasPrefix(key, "include.") ||
		strings.HasPrefix(key, "includif.") ||
		strings.HasPrefix(key, "protocol.") ||
		strings.HasPrefix(key, "url.") && (strings.HasSuffix(key, ".insteadof") || strings.HasSuffix(key, ".pushinsteadof")) ||
		strings.HasPrefix(key, "remote.") && (strings.HasSuffix(key, ".vcs") || strings.HasSuffix(key, ".proxy") || strings.HasSuffix(key, ".uploadpack") || strings.HasSuffix(key, ".receivepack")) ||
		key == "core.gitproxy" || key == "core.sshcommand"
}

func rawOriginURL(ctx context.Context, repository string) (string, error) {
	if err := rejectRepositoryURLAliases(ctx, repository); err != nil {
		return "", err
	}
	command := exec.CommandContext(ctx, "git", "config", "--local", "--get-all", "--no-includes", "remote.origin.url")
	command.Dir = repository
	command.Env = isolatedGitEnvironment(nil)
	output, err := command.Output()
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == 1 {
			return "", errRepositoryOriginAbsent
		}
		return "", err
	}
	values := strings.FieldsFunc(string(output), func(r rune) bool { return r == '\n' || r == '\r' })
	if len(values) != 1 || strings.TrimSpace(values[0]) == "" {
		return "", errors.New("origin must have one exact repository-local URL")
	}
	return strings.TrimSpace(values[0]), nil
}

func rejectRepositoryURLAliases(ctx context.Context, repository string) error {
	command := exec.CommandContext(ctx, "git", "config", "--local", "--null", "--name-only", "--list", "--no-includes")
	command.Dir = repository
	command.Env = isolatedGitEnvironment(nil)
	output, err := command.Output()
	if err != nil {
		return err
	}
	for _, raw := range strings.Split(string(output), "\x00") {
		key := strings.ToLower(strings.TrimSpace(raw))
		if strings.HasPrefix(key, "url.") && (strings.HasSuffix(key, ".insteadof") || strings.HasSuffix(key, ".pushinsteadof")) {
			return fmt.Errorf("%w: URL aliases are not allowed", errUnsafeRepositoryGitConfig)
		}
	}
	return nil
}
