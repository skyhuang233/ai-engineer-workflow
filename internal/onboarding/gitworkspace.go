package onboarding

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type GitCredential struct{ Username, Token string }
type GitWorkspace struct {
	Root, Branch, Head string
	cleanupRoot        string
}

func (w GitWorkspace) Cleanup() error {
	if w.cleanupRoot != "" {
		return os.RemoveAll(w.cleanupRoot)
	}
	if w.Root == "" {
		return nil
	}
	return os.RemoveAll(w.Root)
}

func PrepareOnboardingBranch(ctx context.Context, repository, sourceURL, baseCommit, temporaryRoot, planDigest string, files map[string][]byte, credential GitCredential) (GitWorkspace, error) {
	return prepareOnboardingBranch(ctx, repository, sourceURL, baseCommit, temporaryRoot, planDigest, files, credential, os.RemoveAll)
}

func prepareOnboardingBranch(ctx context.Context, repository, sourceURL, baseCommit, temporaryRoot, planDigest string, files map[string][]byte, credential GitCredential, removeAll func(string) error) (_ GitWorkspace, resultErr error) {
	if sourceURL == "" || !fullSHA.MatchString(baseCommit) || len(planDigest) < 12 || len(files) == 0 {
		return GitWorkspace{}, errors.New("onboarding workspace inputs are incomplete")
	}
	cloneURL := sourceURL
	if credential.Token != "" {
		var err error
		cloneURL, err = GitHubHTTPSURL(repository)
		if err != nil {
			return GitWorkspace{}, err
		}
	}
	root, err := os.MkdirTemp(temporaryRoot, "workflow-onboarding-*")
	if err != nil {
		return GitWorkspace{}, err
	}
	result := GitWorkspace{Root: root, Branch: "workflow/onboarding-" + planDigest[:12]}
	failed := true
	defer func() {
		if failed {
			if cleanupErr := removeAll(root); cleanupErr != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("cleanup temporary onboarding workspace: %w", cleanupErr))
			}
		}
	}()
	env := gitCredentialEnvironment(credential)
	runWithEnvironment := func(dir string, extraEnvironment []string, args ...string) (string, error) {
		command := exec.CommandContext(ctx, "git", args...)
		command.Dir = dir
		command.Env = append(append(append([]string{}, os.Environ()...), env...), extraEnvironment...)
		output, err := command.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
		}
		return strings.TrimSpace(string(output)), nil
	}
	run := func(dir string, args ...string) (string, error) { return runWithEnvironment(dir, nil, args...) }
	clone := filepath.Join(root, "repository")
	hooks := filepath.Join(root, "empty-hooks")
	if err := os.Mkdir(hooks, 0o700); err != nil {
		return GitWorkspace{}, err
	}
	if _, err := run(root, "clone", "--no-checkout", "--origin", "origin", cloneURL, clone); err != nil {
		return GitWorkspace{}, err
	}
	if _, err := run(clone, "checkout", "--detach", baseCommit); err != nil {
		return GitWorkspace{}, err
	}
	if _, err := run(clone, "switch", "-c", result.Branch); err != nil {
		return GitWorkspace{}, err
	}
	for relative, data := range files {
		target := filepath.Join(clone, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return GitWorkspace{}, err
		}
		if err := os.WriteFile(target, data, 0o600); err != nil {
			return GitWorkspace{}, err
		}
	}
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	args := append([]string{"-c", "core.autocrlf=false", "-c", "core.safecrlf=false", "add", "--"}, paths...)
	if _, err := run(clone, args...); err != nil {
		return GitWorkspace{}, err
	}
	commitDate, err := deterministicOnboardingCommitDate(planDigest)
	if err != nil {
		return GitWorkspace{}, err
	}
	if _, err := runWithEnvironment(clone, []string{"GIT_AUTHOR_DATE=" + commitDate, "GIT_COMMITTER_DATE=" + commitDate}, "-c", "user.name=Agent Workflow Setup", "-c", "user.email=workflow@localhost", "-c", "commit.gpgSign=false", "-c", "core.hooksPath="+hooks, "commit", "-m", "Onboard Agent Workflow\n\nSetup-Plan-Digest: "+planDigest); err != nil {
		return GitWorkspace{}, err
	}
	head, err := run(clone, "rev-parse", "HEAD")
	if err != nil {
		return GitWorkspace{}, err
	}
	if _, err := run(clone, "push", "origin", "HEAD:refs/heads/"+result.Branch); err != nil {
		return GitWorkspace{}, err
	}
	result.Root = clone
	result.Head = head
	result.cleanupRoot = root
	failed = false
	return result, nil
}

func deterministicOnboardingCommitDate(planDigest string) (string, error) {
	decoded, err := hex.DecodeString(planDigest)
	if err != nil || len(decoded) < 8 {
		return "", errors.New("onboarding plan digest cannot derive a deterministic commit date")
	}
	const fiftyYears = uint64(50 * 365 * 24 * 60 * 60)
	seconds := binary.BigEndian.Uint64(decoded[:8]) % fiftyYears
	return time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(seconds) * time.Second).Format(time.RFC3339), nil
}
func gitCredentialEnvironment(value GitCredential) []string {
	if value.Token == "" {
		return nil
	}
	username := value.Username
	if username == "" {
		username = "x-access-token"
	}
	authorization := base64.StdEncoding.EncodeToString([]byte(username + ":" + value.Token))
	return []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.extraHeader", "GIT_CONFIG_VALUE_0=Authorization: Basic " + authorization, "GIT_TERMINAL_PROMPT=0"}
}
