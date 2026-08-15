package onboarding

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type capturedGitProcess struct {
	Args []string `json:"args"`
	Env  []string `json:"env"`
}

func TestMain(m *testing.M) {
	switch strings.ToLower(filepath.Base(os.Args[0])) {
	case "git", "git.exe":
		if os.Getenv("WORKFLOW_TEST_REAL_GIT") != "" {
			os.Exit(runCapturedGit())
		}
	case "malicious-filter", "malicious-filter.exe":
		os.Exit(runCapturedFilter())
	}
	os.Exit(m.Run())
}

func runCapturedGit() int {
	if err := appendCapturedProcess(os.Getenv("WORKFLOW_TEST_GIT_CAPTURE"), capturedGitProcess{Args: os.Args[1:], Env: os.Environ()}); err != nil {
		return 125
	}
	args := append([]string{}, os.Args[1:]...)
	canonicalURL := os.Getenv("WORKFLOW_TEST_CANONICAL_URL")
	for index := range args {
		if args[index] == canonicalURL {
			args[index] = os.Getenv("WORKFLOW_TEST_LOCAL_REMOTE")
		}
	}
	if capturedGitSubcommand(args) == "push" {
		for index := range args {
			if args[index] == "origin" {
				args[index] = os.Getenv("WORKFLOW_TEST_LOCAL_REMOTE")
			}
		}
	}
	command := exec.Command(os.Getenv("WORKFLOW_TEST_REAL_GIT"), args...)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode()
		}
		return 125
	}
	if capturedGitSubcommand(args) == "clone" {
		cloneRoot := args[len(args)-1]
		filterCommand := `"` + filepath.ToSlash(os.Getenv("WORKFLOW_TEST_FILTER_EXE")) + `"`
		for _, filterType := range []string{"clean", "smudge"} {
			config := exec.Command(os.Getenv("WORKFLOW_TEST_REAL_GIT"), "-C", cloneRoot, "config", "filter.malicious."+filterType, filterCommand)
			if err := config.Run(); err != nil {
				return 125
			}
		}
		config := exec.Command(os.Getenv("WORKFLOW_TEST_REAL_GIT"), "-C", cloneRoot, "config", "filter.malicious.required", "true")
		if err := config.Run(); err != nil {
			return 125
		}
	}
	return 0
}

func runCapturedFilter() int {
	if capturePath := os.Getenv("WORKFLOW_TEST_FILTER_CAPTURE"); capturePath != "" {
		if err := appendCapturedProcess(capturePath, capturedGitProcess{Args: os.Args[1:], Env: os.Environ()}); err != nil {
			return 125
		}
	}
	if replacement := os.Getenv("WORKFLOW_TEST_FILTER_REPLACEMENT"); replacement != "" {
		if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
			return 125
		}
		if _, err := io.WriteString(os.Stdout, replacement); err != nil {
			return 125
		}
		return 0
	}
	if _, err := io.Copy(os.Stdout, os.Stdin); err != nil {
		return 125
	}
	return 0
}

func appendCapturedProcess(path string, capture capturedGitProcess) error {
	encoded, err := json.Marshal(capture)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.Write(append(encoded, '\n'))
	return err
}

func capturedGitSubcommand(args []string) string {
	for index := 0; index < len(args); index++ {
		if args[index] == "-c" && index+1 < len(args) {
			index++
			continue
		}
		return args[index]
	}
	return ""
}

func TestPrepareOnboardingBranchRestrictsPATToCanonicalNetworkOperations(t *testing.T) {
	source := newRepo(t)
	if err := os.WriteFile(filepath.Join(source, ".gitattributes"), []byte("managed.txt filter=malicious\nAGENTS.md filter=malicious\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, source, "add", ".gitattributes")
	git(t, source, "commit", "-m", "declare malicious filter")
	bare := filepath.Join(t.TempDir(), "remote.git")
	git(t, "", "clone", "--bare", source, bare)
	base := testGitOutput(t, source, "rev-parse", "HEAD")

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	testExecutable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	shimRoot := t.TempDir()
	gitShim := filepath.Join(shimRoot, "git")
	filterShim := filepath.Join(shimRoot, "malicious-filter")
	if runtime.GOOS == "windows" {
		gitShim += ".exe"
		filterShim += ".exe"
	}
	for _, target := range []string{gitShim, filterShim} {
		data, readErr := os.ReadFile(testExecutable)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if writeErr := os.WriteFile(target, data, 0o700); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	canonicalURL := "https://github.com/owner/repo.git"
	gitCapture := filepath.Join(t.TempDir(), "git-processes.jsonl")
	filterCapture := filepath.Join(t.TempDir(), "filter-processes.jsonl")
	t.Setenv("WORKFLOW_TEST_REAL_GIT", realGit)
	t.Setenv("WORKFLOW_TEST_CANONICAL_URL", canonicalURL)
	t.Setenv("WORKFLOW_TEST_LOCAL_REMOTE", bare)
	t.Setenv("WORKFLOW_TEST_GIT_CAPTURE", gitCapture)
	t.Setenv("WORKFLOW_TEST_FILTER_CAPTURE", filterCapture)
	t.Setenv("WORKFLOW_TEST_FILTER_EXE", filterShim)
	t.Setenv("WORKFLOW_TEST_FILTER_REPLACEMENT", "attacker-controlled bytes\n")
	t.Setenv("PATH", shimRoot+string(os.PathListSeparator)+os.Getenv("PATH"))
	if output, err := exec.Command(filterShim).CombinedOutput(); err != nil {
		t.Fatalf("filter test process cannot start: %v (%s)", err, output)
	}
	if err := os.Remove(filterCapture); err != nil {
		t.Fatal(err)
	}

	credential := GitCredential{Username: "setup-user", Token: "github_pat_round12_exact_secret"}
	agents := []byte("user instructions before\n" + ManagedBlockStart + "\nmanaged contract\n" + ManagedBlockEnd + "\nuser instructions after\n")
	approvedFiles := map[string][]byte{"managed.txt": []byte("contract\n"), "AGENTS.md": agents}
	workspace, err := PrepareOnboardingBranch(context.Background(), "owner/repo", bare, base, t.TempDir(), repeatString("a", 64), approvedFiles, credential)
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Cleanup()

	encodedCredential := base64.StdEncoding.EncodeToString([]byte(credential.Username + ":" + credential.Token))
	wantedScopedKey := "GIT_CONFIG_KEY_0=http." + canonicalURL + ".extraHeader"
	captures := readCapturedProcesses(t, gitCapture)
	seenNetwork := map[string]bool{}
	for _, capture := range captures {
		subcommand := capturedGitSubcommand(capture.Args)
		serialized := strings.Join(append(append([]string{}, capture.Args...), capture.Env...), "\n")
		switch subcommand {
		case "clone", "push", "fetch":
			seenNetwork[subcommand] = true
			for _, wanted := range []string{wantedScopedKey, "GIT_CONFIG_VALUE_0=Authorization: Basic " + encodedCredential} {
				if !strings.Contains(serialized, wanted) {
					t.Fatalf("%s lacks scoped credential environment %q", subcommand, wanted)
				}
			}
		default:
			for _, forbidden := range []string{credential.Token, encodedCredential, "Authorization: Basic", "extraHeader"} {
				if strings.Contains(serialized, forbidden) {
					t.Fatalf("local git %s received credential material %q", subcommand, forbidden)
				}
			}
		}
	}
	for _, wanted := range []string{"clone", "push"} {
		if !seenNetwork[wanted] {
			t.Fatalf("network operation %s was not observed", wanted)
		}
	}
	for path, approved := range approvedFiles {
		committed := exec.Command(realGit, "-C", workspace.Root, "show", workspace.Head+":"+path)
		committedBytes, err := committed.Output()
		if err != nil || string(committedBytes) != string(approved) {
			t.Fatalf("approved bytes for %s were filtered: %q, %v", path, committedBytes, err)
		}
	}
	if captures, err := os.ReadFile(filterCapture); err == nil && len(captures) > 0 {
		t.Fatalf("repository-controlled clean filter executed while constructing approved commit: %s", captures)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	localConfig, err := os.ReadFile(filepath.Join(workspace.Root, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{credential.Token, encodedCredential, "authorization", "extraheader"} {
		if strings.Contains(strings.ToLower(string(localConfig)), strings.ToLower(forbidden)) {
			t.Fatalf("local repository config persisted credential material %q", forbidden)
		}
	}
}

func readCapturedProcesses(t *testing.T, path string) []capturedGitProcess {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var captures []capturedGitProcess
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var capture capturedGitProcess
		if err := json.Unmarshal([]byte(line), &capture); err != nil {
			t.Fatal(err)
		}
		captures = append(captures, capture)
	}
	return captures
}

func TestPrepareOnboardingBranchUsesTemporaryCloneOnly(t *testing.T) {
	source := newRepo(t)
	bare := filepath.Join(t.TempDir(), "remote.git")
	git(t, "", "clone", "--bare", source, bare)
	base := testGitOutput(t, source, "rev-parse", "HEAD")
	dirty := filepath.Join(source, "local.txt")
	if err := os.WriteFile(dirty, []byte("user owned"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace, err := PrepareOnboardingBranch(context.Background(), "owner/repo", bare, base, t.TempDir(), repeatString("a", 64), map[string][]byte{"managed.txt": []byte("contract\n")}, GitCredential{})
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Cleanup()
	if workspace.Head == base || workspace.Branch != "workflow/onboarding-aaaaaaaaaaaa" {
		t.Fatalf("workspace=%#v", workspace)
	}
	if data, err := os.ReadFile(dirty); err != nil || string(data) != "user owned" {
		t.Fatal("source working copy changed")
	}
	remoteHead := testGitOutput(t, "", "--git-dir", bare, "rev-parse", "refs/heads/"+workspace.Branch)
	if remoteHead != workspace.Head {
		t.Fatalf("remote=%s head=%s", remoteHead, workspace.Head)
	}
}

func TestPrepareOnboardingBranchPreservesCloneAndCleanupFailures(t *testing.T) {
	cleanupCalled := false
	_, err := prepareOnboardingBranch(
		context.Background(),
		"owner/repo",
		filepath.Join(t.TempDir(), "missing.git"),
		repeatString("b", 40),
		t.TempDir(),
		repeatString("a", 64),
		map[string][]byte{"managed.txt": []byte("contract\n")},
		GitCredential{},
		func(string) error {
			cleanupCalled = true
			return errors.New("temporary workspace removal denied")
		},
	)
	if err == nil {
		t.Fatal("clone preparation reported success")
	}
	for _, wanted := range []string{"git clone", "temporary workspace removal denied"} {
		if !strings.Contains(err.Error(), wanted) {
			t.Fatalf("combined error %q lacks %q", err, wanted)
		}
	}
	if !cleanupCalled {
		t.Fatal("temporary workspace cleanup was not attempted")
	}
}

func TestPrepareOnboardingBranchConvergesToDeterministicCommitAndRejectsThirdState(t *testing.T) {
	source := newRepo(t)
	for _, key := range []string{
		"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_AUTHOR_DATE",
		"GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL", "GIT_COMMITTER_DATE",
		"GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0",
		"GIT_CONFIG_GLOBAL", "GIT_CONFIG_NOSYSTEM", "GIT_ATTR_NOSYSTEM",
	} {
		unsetEnvironmentForTest(t, key)
	}
	base := testGitOutput(t, source, "rev-parse", "HEAD")
	bare := filepath.Join(t.TempDir(), "remote.git")
	git(t, "", "clone", "--bare", source, bare)
	digest := repeatString("a", 64)
	files := map[string][]byte{"managed.txt": []byte("contract\n")}
	first, err := PrepareOnboardingBranch(context.Background(), "owner/repo", bare, base, t.TempDir(), digest, files, GitCredential{})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Cleanup()

	globalHooks := filepath.Join(t.TempDir(), "global-hooks")
	if err := os.Mkdir(globalHooks, 0o700); err != nil {
		t.Fatal(err)
	}
	hookSideEffect := filepath.Join(t.TempDir(), "unapproved-hook-ran")
	if err := os.WriteFile(filepath.Join(globalHooks, "pre-commit"), []byte("#!/bin/sh\nprintf invoked > \"$WORKFLOW_TEST_HOOK_SIDE_EFFECT\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	testExecutable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	filterShim := filepath.Join(t.TempDir(), "malicious-filter")
	if runtime.GOOS == "windows" {
		filterShim += ".exe"
	}
	executableData, err := os.ReadFile(testExecutable)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filterShim, executableData, 0o700); err != nil {
		t.Fatal(err)
	}
	globalAttributes := filepath.Join(t.TempDir(), "global-attributes")
	if err := os.WriteFile(globalAttributes, []byte("managed.txt filter=malicious\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	globalConfig := filepath.Join(t.TempDir(), "gitconfig")
	globalConfigContents := "[user]\n\tname = Hostile Global Author\n\temail = hostile-global@example.invalid\n" +
		"[core]\n\tautocrlf = true\n\thooksPath = " + filepath.ToSlash(globalHooks) + "\n\tattributesFile = " + filepath.ToSlash(globalAttributes) + "\n" +
		"[commit]\n\tgpgSign = true\n" +
		"[filter \"malicious\"]\n\tclean = " + filepath.ToSlash(filterShim) + "\n\trequired = true\n" +
		"[i18n]\n\tcommitEncoding = ISO-8859-1\n"
	if err := os.WriteFile(globalConfig, []byte(globalConfigContents), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalConfig)
	t.Setenv("WORKFLOW_TEST_HOOK_SIDE_EFFECT", hookSideEffect)
	t.Setenv("WORKFLOW_TEST_FILTER_REPLACEMENT", "hostile-filter-output\n")
	t.Setenv("GIT_AUTHOR_NAME", "Hostile Environment Author")
	t.Setenv("GIT_AUTHOR_EMAIL", "hostile-author@example.invalid")
	t.Setenv("GIT_AUTHOR_DATE", "2001-01-01T00:00:00Z")
	t.Setenv("GIT_COMMITTER_NAME", "Hostile Environment Committer")
	t.Setenv("GIT_COMMITTER_EMAIL", "hostile-committer@example.invalid")
	t.Setenv("GIT_COMMITTER_DATE", "2002-02-02T00:00:00Z")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.autocrlf")
	t.Setenv("GIT_CONFIG_VALUE_0", "true")
	second, err := PrepareOnboardingBranch(context.Background(), "owner/repo", bare, base, t.TempDir(), digest, files, GitCredential{})
	if err != nil {
		t.Fatalf("same approved branch retry did not converge: %v", err)
	}
	defer second.Cleanup()
	if first.Head != second.Head {
		t.Fatalf("same plan/source/tree produced %s then %s", first.Head, second.Head)
	}
	if _, err := os.Stat(hookSideEffect); !os.IsNotExist(err) {
		t.Fatalf("onboarding commit executed an unapproved user Git hook: %v", err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_COUNT", "0")

	attacker := filepath.Join(t.TempDir(), "third-state")
	git(t, "", "clone", "--branch", first.Branch, bare, attacker)
	git(t, attacker, "config", "user.name", "Other")
	git(t, attacker, "config", "user.email", "other@example.com")
	if err := os.WriteFile(filepath.Join(attacker, "unapproved.txt"), []byte("third state\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, attacker, "add", "unapproved.txt")
	git(t, attacker, "commit", "-m", "unapproved")
	git(t, attacker, "push", "origin", "HEAD:"+first.Branch)
	if _, err := PrepareOnboardingBranch(context.Background(), "owner/repo", bare, base, t.TempDir(), digest, files, GitCredential{}); err == nil || !strings.Contains(err.Error(), "push") {
		t.Fatalf("external onboarding branch third state was accepted: %v", err)
	}
}

func unsetEnvironmentForTest(t *testing.T, key string) {
	t.Helper()
	value, present := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if present {
			_ = os.Setenv(key, value)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

func repeatString(value string, count int) string {
	result := ""
	for i := 0; i < count; i++ {
		result += value
	}
	return result
}
