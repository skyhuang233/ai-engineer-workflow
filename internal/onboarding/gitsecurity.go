package onboarding

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strings"
)

var errUnsafeRepositoryGitConfig = errors.New("unsafe repository-local Git configuration")
var errRepositoryOriginAbsent = errors.New("repository has no local origin URL")

// ValidateAuthenticatedGitRepository rejects repository-owned configuration
// that could redirect or weaken a later PAT-bearing Git transport operation.
func ValidateAuthenticatedGitRepository(ctx context.Context, repository string) error {
	if _, _, err := currentHostProxyEnvironment(); err != nil {
		return err
	}
	return ValidateLocalGitReadConfiguration(ctx, repository)
}

// ValidateLocalGitReadConfiguration rejects repository-owned Git settings
// that can execute code or redirect later reads away from repository.
func ValidateLocalGitReadConfiguration(ctx context.Context, repository string) error {
	if err := validateRepositoryGitConfigScope(ctx, repository, "local"); err != nil {
		return err
	}
	enabled, err := repositoryWorktreeConfigEnabled(ctx, repository)
	if err != nil {
		return err
	}
	if !enabled {
		return nil
	}
	return validateRepositoryGitConfigScope(ctx, repository, "worktree")
}

func repositoryWorktreeConfigEnabled(ctx context.Context, repository string) (bool, error) {
	command := exec.CommandContext(ctx, "git", "config", "--local", "--no-includes", "--bool", "--get", "extensions.worktreeConfig")
	command.Dir = repository
	command.Env = isolatedGitEnvironment([]string{"GIT_OPTIONAL_LOCKS=0"})
	output, err := command.Output()
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == 1 {
			return false, nil
		}
		return false, fmt.Errorf("inspect repository-local worktree configuration extension: %w", err)
	}
	return strings.TrimSpace(string(output)) == "true", nil
}

func validateRepositoryGitConfigScope(ctx context.Context, repository, scope string) error {
	command := exec.CommandContext(ctx, "git", "config", "--"+scope, "--no-includes", "-z", "--name-only", "--get-regexp", ".*")
	command.Dir = repository
	command.Env = isolatedGitEnvironment([]string{"GIT_OPTIONAL_LOCKS=0"})
	output, err := command.Output()
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == 1 {
			return nil
		}
		return fmt.Errorf("inspect repository-%s Git configuration: %w", scope, err)
	}
	for _, raw := range strings.Split(string(output), "\x00") {
		key := strings.ToLower(strings.TrimSpace(raw))
		if key == "" {
			continue
		}
		if unsafeRepositoryGitKey(key) {
			return fmt.Errorf("%w: %s key %q is unsafe for authenticated transport", errUnsafeRepositoryGitConfig, scope, raw)
		}
	}
	return nil
}

func unsafeRepositoryGitKey(key string) bool {
	return strings.HasPrefix(key, "http.") ||
		strings.HasPrefix(key, "https.") ||
		strings.HasPrefix(key, "credential.") ||
		strings.HasPrefix(key, "include.") ||
		strings.HasPrefix(key, "includeif.") ||
		strings.HasPrefix(key, "protocol.") ||
		strings.HasPrefix(key, "filter.") ||
		strings.HasPrefix(key, "diff.") && (strings.HasSuffix(key, ".textconv") || strings.HasSuffix(key, ".external") || strings.HasSuffix(key, ".command")) ||
		strings.HasPrefix(key, "merge.") && strings.HasSuffix(key, ".driver") ||
		strings.HasPrefix(key, "url.") && (strings.HasSuffix(key, ".insteadof") || strings.HasSuffix(key, ".pushinsteadof")) ||
		strings.HasPrefix(key, "remote.") && (strings.HasSuffix(key, ".vcs") || strings.HasSuffix(key, ".proxy") || strings.HasSuffix(key, ".uploadpack") || strings.HasSuffix(key, ".receivepack")) ||
		key == "core.gitproxy" || key == "core.sshcommand" || key == "core.fsmonitor" || key == "core.attributesfile" || key == "core.hookspath"
}

// ReadLocalOriginURL returns exactly one repository-local origin without
// applying URL aliases or inheriting process Git configuration redirects.
func ReadLocalOriginURL(ctx context.Context, repository string) (string, error) {
	if err := rejectRepositoryURLAliases(ctx, repository); err != nil {
		return "", err
	}
	command := exec.CommandContext(ctx, "git", "config", "--local", "--get-all", "--no-includes", "remote.origin.url")
	command.Dir = repository
	command.Env = isolatedGitEnvironment([]string{"GIT_OPTIONAL_LOCKS=0"})
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

func rawOriginURL(ctx context.Context, repository string) (string, error) {
	return ReadLocalOriginURL(ctx, repository)
}

func rejectRepositoryURLAliases(ctx context.Context, repository string) error {
	return ValidateLocalGitReadConfiguration(ctx, repository)
}

type hostProxySnapshot struct {
	DigestSHA256      string
	RedactedEndpoints []string
	NoProxyConfigured bool
}

var allowedProxyVariables = map[string]bool{"HTTP_PROXY": true, "HTTPS_PROXY": true, "ALL_PROXY": true, "NO_PROXY": true}

func currentHostProxyEnvironment() (hostProxySnapshot, []string, error) {
	values := map[string]string{}
	entries := map[string]string{}
	for _, entry := range os.Environ() {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		upper := strings.ToUpper(name)
		if !allowedProxyVariables[upper] {
			continue
		}
		if value == "" {
			continue
		}
		if _, duplicate := values[upper]; duplicate {
			return hostProxySnapshot{}, nil, errors.New("host proxy environment contains duplicate case-insensitive variables")
		}
		values[upper], entries[upper] = value, entry
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	var exact strings.Builder
	result := hostProxySnapshot{}
	preserved := make([]string, 0, len(names))
	for _, name := range names {
		value := values[name]
		exact.WriteString(name + "=" + value + "\n")
		preserved = append(preserved, entries[name])
		if name == "NO_PROXY" {
			result.NoProxyConfigured = value != ""
			continue
		}
		if value == "" {
			continue
		}
		parsed, err := url.Parse(value)
		if err != nil || parsed.Hostname() == "" || parsed.Scheme == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
			return hostProxySnapshot{}, nil, fmt.Errorf("%s is not a supported absolute proxy endpoint", name)
		}
		scheme := strings.ToLower(parsed.Scheme)
		if scheme != "http" && scheme != "https" && scheme != "socks5" && scheme != "socks5h" {
			return hostProxySnapshot{}, nil, fmt.Errorf("%s uses an unsupported proxy scheme", name)
		}
		endpoint := name + "=" + scheme + "://" + parsed.Hostname()
		if parsed.Port() != "" {
			endpoint += ":" + parsed.Port()
		}
		result.RedactedEndpoints = append(result.RedactedEndpoints, endpoint)
	}
	sum := sha256.Sum256([]byte(exact.String()))
	result.DigestSHA256 = hex.EncodeToString(sum[:])
	return result, preserved, nil
}
