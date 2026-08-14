package onboarding

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func PrepareOnboardingBranch(ctx context.Context, sourceURL, baseCommit, temporaryRoot, planDigest string, files map[string][]byte, credential GitCredential) (GitWorkspace, error) {
	if sourceURL == "" || !fullSHA.MatchString(baseCommit) || len(planDigest) < 12 || len(files) == 0 {
		return GitWorkspace{}, errors.New("onboarding workspace inputs are incomplete")
	}
	root, err := os.MkdirTemp(temporaryRoot, "workflow-onboarding-*")
	if err != nil {
		return GitWorkspace{}, err
	}
	result := GitWorkspace{Root: root, Branch: "workflow/onboarding-" + planDigest[:12]}
	failed := true
	defer func() {
		if failed {
			_ = os.RemoveAll(root)
		}
	}()
	env := gitCredentialEnvironment(credential)
	run := func(dir string, args ...string) (string, error) {
		command := exec.CommandContext(ctx, "git", args...)
		command.Dir = dir
		command.Env = append(os.Environ(), env...)
		output, err := command.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
		}
		return strings.TrimSpace(string(output)), nil
	}
	clone := filepath.Join(root, "repository")
	if _, err := run(root, "clone", "--no-checkout", "--origin", "origin", sourceURL, clone); err != nil {
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
	args := append([]string{"add", "--"}, paths...)
	if _, err := run(clone, args...); err != nil {
		return GitWorkspace{}, err
	}
	if _, err := run(clone, "-c", "user.name=Agent Workflow Setup", "-c", "user.email=workflow@localhost", "commit", "-m", "Onboard Agent Workflow\n\nSetup-Plan-Digest: "+planDigest); err != nil {
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
