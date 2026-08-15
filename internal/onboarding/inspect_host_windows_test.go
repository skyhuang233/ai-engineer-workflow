package onboarding

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestPowerShellHostInspectionRequiresExactlyOneRawLocalOrigin(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "windows" {
		t.Skip("Windows PowerShell 5.1 is the supported bootstrap shell")
	}
	powershell, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("Windows PowerShell 5.1 is unavailable")
	}
	_, currentFile, _, _ := runtime.Caller(0)
	script := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", "..", "skills", "setup-agent-workflow", "scripts", "inspect-host.ps1"))

	run := func(t *testing.T, repo string, hostileEnv ...string) ([]byte, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, powershell, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", script, "-Repository", repo, "-GitProbeOnly")
		command.Env = append(os.Environ(), hostileEnv...)
		return command.CombinedOutput()
	}

	t.Run("absent", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		git(t, repo, "remote", "remove", "origin")
		output, err := run(t, repo)
		if err != nil {
			t.Fatalf("inspect absent origin: %v\n%s", err, output)
		}
		var facts struct {
			Origin string `json:"origin"`
		}
		if err := json.Unmarshal(output, &facts); err != nil || facts.Origin != "" {
			t.Fatalf("absent origin facts = %#v, %v, output=%s", facts, err, output)
		}
	})

	t.Run("unborn repository", func(t *testing.T) {
		t.Parallel()
		repo := filepath.Join(t.TempDir(), "unborn")
		git(t, "", "init", "-b", "main", repo)
		output, err := run(t, repo)
		if err != nil {
			t.Fatalf("inspect unborn repository: %v\n%s", err, output)
		}
		var facts struct {
			IsRepository bool   `json:"is_repository"`
			Branch       string `json:"branch"`
			Head         string `json:"head"`
		}
		if err := json.Unmarshal(output, &facts); err != nil || !facts.IsRepository || facts.Branch != "main" || facts.Head != "" {
			t.Fatalf("unborn repository facts = %#v, %v, output=%s", facts, err, output)
		}
	})

	t.Run("one raw local value ignores hostile config", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		raw := "git@github.com:owner/repo.git"
		git(t, repo, "config", "--local", "remote.origin.url", raw)
		output, err := run(t, repo, "GIT_DIR=C:\\attacker", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=remote.origin.url", "GIT_CONFIG_VALUE_0=https://attacker.invalid/repo.git")
		if err != nil {
			t.Fatalf("inspect local origin: %v\n%s", err, output)
		}
		var facts struct {
			Origin string `json:"origin"`
		}
		if err := json.Unmarshal(output, &facts); err != nil || facts.Origin != raw {
			t.Fatalf("raw local origin facts = %#v, %v, output=%s", facts, err, output)
		}
	})

	t.Run("rejects insteadOf before origin read", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		raw := "owner-shortcut:repo"
		git(t, repo, "config", "--local", "remote.origin.url", raw)
		git(t, repo, "config", "--local", "url.https://github.com/owner/.insteadOf", "owner-shortcut:")
		output, err := run(t, repo)
		if err == nil || !strings.Contains(string(output), "unsafe repository-local Git configuration") {
			t.Fatalf("insteadOf was not rejected: %v\n%s", err, output)
		}
	})

	t.Run("multiple local values block", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		git(t, repo, "config", "--local", "--add", "remote.origin.url", "https://github.com/owner/one.git")
		git(t, repo, "config", "--local", "--add", "remote.origin.url", "https://github.com/owner/two.git")
		output, err := run(t, repo)
		if err == nil || !strings.Contains(string(output), "exactly one local remote.origin.url") {
			t.Fatalf("multiple origins were not blocked: %v\n%s", err, output)
		}
	})

	t.Run("raw local whitespace blocks", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		git(t, repo, "config", "--local", "remote.origin.url", " https://github.com/owner/repo.git ")
		output, err := run(t, repo)
		if err == nil || !strings.Contains(string(output), "exactly one local remote.origin.url") {
			t.Fatalf("whitespace-bearing raw origin was normalized: %v\n%s", err, output)
		}
	})

	t.Run("raw local newline whitespace blocks", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		git(t, repo, "config", "--local", "remote.origin.url", "\nhttps://github.com/owner/repo.git")
		output, err := run(t, repo)
		if err == nil || !strings.Contains(string(output), "exactly one local remote.origin.url") {
			t.Fatalf("newline-bearing raw origin was normalized: %v\n%s", err, output)
		}
	})

	t.Run("disabled worktree config is absent", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		git(t, repo, "config", "--local", "extensions.worktreeConfig", "true")
		git(t, repo, "config", "--worktree", "credential.helper", "stale-helper")
		git(t, repo, "config", "--local", "extensions.worktreeConfig", "false")
		output, err := run(t, repo)
		if err != nil {
			t.Fatalf("disabled worktree config was not treated as absent: %v\n%s", err, output)
		}
	})

	t.Run("multiple worktree config declarations block", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		git(t, repo, "config", "--local", "--add", "extensions.worktreeConfig", "true")
		git(t, repo, "config", "--local", "--add", "extensions.worktreeConfig", "")
		output, err := run(t, repo)
		if err == nil || !strings.Contains(string(output), "expected zero or exactly one extensions.worktreeConfig value") {
			t.Fatalf("multiple worktree config declarations were not blocked: %v\n%s", err, output)
		}
	})

	t.Run("noncanonical worktree config declaration blocks", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		git(t, repo, "config", "--local", "extensions.worktreeConfig", "yes")
		output, err := run(t, repo)
		if err == nil || !strings.Contains(string(output), "extensions.worktreeConfig must be true or false") {
			t.Fatalf("noncanonical worktree config declaration was not blocked: %v\n%s", err, output)
		}
	})

	for _, test := range []struct {
		name, scope, key, value string
	}{
		{name: "dangerous local config", scope: "--local", key: "core.hooksPath", value: filepath.Join(t.TempDir(), "hooks")},
		{name: "dangerous local worktree redirect", scope: "--local", key: "core.worktree", value: t.TempDir()},
		{name: "dangerous local alternate refs command", scope: "--local", key: "core.alternateRefsCommand", value: "attacker-command"},
		{name: "dangerous local includeIf", scope: "--local", key: "includeIf.gitdir:C:/attacker.path", value: filepath.Join(t.TempDir(), "attacker.config")},
		{name: "dangerous worktree config", scope: "--worktree", key: "credential.helper", value: "attacker-helper"},
		{name: "dangerous worktree redirect", scope: "--worktree", key: "core.worktree", value: t.TempDir()},
		{name: "dangerous worktree alternate refs command", scope: "--worktree", key: "core.alternateRefsCommand", value: "attacker-command"},
		{name: "dangerous worktree includeIf", scope: "--worktree", key: "includeIf.gitdir:C:/attacker.path", value: filepath.Join(t.TempDir(), "attacker.config")},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			repo := newRepo(t)
			if test.scope == "--worktree" {
				git(t, repo, "config", "--local", "extensions.worktreeConfig", "true")
			}
			git(t, repo, "config", test.scope, test.key, test.value)
			output, err := run(t, repo)
			if err == nil || !strings.Contains(string(output), "unsafe repository-local Git configuration") {
				t.Fatalf("dangerous %s config was not blocked: %v\n%s", test.scope, err, output)
			}
		})
	}
}

func TestPowerShellHostInspectionDoesNotRefreshOrLockGitIndex(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "windows" {
		t.Skip("Windows PowerShell 5.1 is the supported bootstrap shell")
	}
	powershell, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("Windows PowerShell 5.1 is unavailable")
	}
	repo := newRepo(t)
	index := filepath.Join(repo, ".git", "index")
	before, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	fixed := time.Unix(1_700_000_000, 0)
	if err := os.Chtimes(index, fixed, fixed); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(repo, "README.md"), fixed.Add(2*time.Hour), fixed.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	_, currentFile, _, _ := runtime.Caller(0)
	script := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", "..", "skills", "setup-agent-workflow", "scripts", "inspect-host.ps1"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, powershell, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", script, "-Repository", repo, "-GitProbeOnly")
	for _, variable := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(variable), "GIT_OPTIONAL_LOCKS=") {
			command.Env = append(command.Env, variable)
		}
	}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("inspect-host.ps1: %v\n%s", err, output)
	}
	after, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(index)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) || !info.ModTime().Equal(fixed) {
		t.Fatalf("PowerShell read-only inspection refreshed index: bytes_changed=%t mtime=%s", !bytes.Equal(after, before), info.ModTime())
	}
	if _, err := os.Stat(index + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("PowerShell read-only inspection left index.lock: %v", err)
	}
}
