package onboarding

import (
	"bytes"
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
		command := exec.Command(powershell, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", script, "-Repository", repo, "-WorkflowHome", filepath.Join(t.TempDir(), "workflow-home"))
		command.Env = append(os.Environ(), hostileEnv...)
		return command.CombinedOutput()
	}

	t.Run("absent", func(t *testing.T) {
		repo := newRepo(t)
		git(t, repo, "remote", "remove", "origin")
		output, err := run(t, repo)
		if err != nil {
			t.Fatalf("inspect absent origin: %v\n%s", err, output)
		}
		var facts struct {
			Git struct {
				Origin string `json:"origin"`
			} `json:"git"`
		}
		if err := json.Unmarshal(output, &facts); err != nil || facts.Git.Origin != "" {
			t.Fatalf("absent origin facts = %#v, %v, output=%s", facts, err, output)
		}
	})

	t.Run("one raw local value ignores hostile config", func(t *testing.T) {
		repo := newRepo(t)
		raw := "git@github.com:owner/repo.git"
		git(t, repo, "config", "--local", "remote.origin.url", raw)
		output, err := run(t, repo, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=remote.origin.url", "GIT_CONFIG_VALUE_0=https://attacker.invalid/repo.git")
		if err != nil {
			t.Fatalf("inspect local origin: %v\n%s", err, output)
		}
		var facts struct {
			Git struct {
				Origin string `json:"origin"`
			} `json:"git"`
		}
		if err := json.Unmarshal(output, &facts); err != nil || facts.Git.Origin != raw {
			t.Fatalf("raw local origin facts = %#v, %v, output=%s", facts, err, output)
		}
	})

	t.Run("does not expand insteadOf", func(t *testing.T) {
		repo := newRepo(t)
		raw := "owner-shortcut:repo"
		git(t, repo, "config", "--local", "remote.origin.url", raw)
		git(t, repo, "config", "--local", "url.https://github.com/owner/.insteadOf", "owner-shortcut:")
		output, err := run(t, repo)
		if err != nil {
			t.Fatalf("inspect raw insteadOf origin: %v\n%s", err, output)
		}
		var facts struct {
			Git struct {
				Origin string `json:"origin"`
			} `json:"git"`
		}
		if err := json.Unmarshal(output, &facts); err != nil || facts.Git.Origin != raw {
			t.Fatalf("insteadOf changed raw local origin: %#v, %v, output=%s", facts, err, output)
		}
	})

	t.Run("multiple local values block", func(t *testing.T) {
		repo := newRepo(t)
		git(t, repo, "config", "--local", "--add", "remote.origin.url", "https://github.com/owner/one.git")
		git(t, repo, "config", "--local", "--add", "remote.origin.url", "https://github.com/owner/two.git")
		output, err := run(t, repo)
		if err == nil || !strings.Contains(string(output), "exactly one local remote.origin.url") {
			t.Fatalf("multiple origins were not blocked: %v\n%s", err, output)
		}
	})
}

func TestPowerShellHostInspectionDoesNotRefreshOrLockGitIndex(t *testing.T) {
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
	command := exec.Command(powershell, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", script, "-Repository", repo, "-WorkflowHome", filepath.Join(t.TempDir(), "workflow-home"))
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
