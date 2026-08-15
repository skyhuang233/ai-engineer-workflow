package onboarding

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestInitialBaselineUsesTemporaryIndexAndExactApprovedFiles(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	git(t, "", "init", "-b", "main", repo)
	git(t, repo, "config", "user.email", "test@example.com")
	git(t, repo, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "staged.txt"), []byte("staged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "staged.txt")
	if err := os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("untracked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "ignored.txt"), []byte("ignored\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("ignored.txt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(repo, ".git", "index")
	before := readOptional(t, indexPath)
	files, err := BaselineFiles(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{".gitignore", "staged.txt", "untracked.txt"}
	if !reflect.DeepEqual(files, want) {
		t.Fatalf("files=%v want=%v", files, want)
	}
	snapshot, err := BaselineSnapshot(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	commit, err := CreateInitialBaseline(context.Background(), repo, "main", snapshot, "Initial repository baseline")
	if err != nil {
		t.Fatal(err)
	}
	if len(commit) != 40 {
		t.Fatalf("commit=%q", commit)
	}
	after := readOptional(t, indexPath)
	if string(before) != string(after) {
		t.Fatal("user index bytes changed")
	}
	tree := testGitOutput(t, repo, "ls-tree", "-r", "--name-only", "HEAD")
	if tree != ".gitignore\nstaged.txt\nuntracked.txt" {
		t.Fatalf("tree=%q", tree)
	}
	if author := testGitOutput(t, repo, "show", "-s", "--format=%an <%ae>", "HEAD"); author != "Agent Workflow Setup <workflow@localhost>" {
		t.Fatalf("baseline author = %q", author)
	}
}

func TestBaselineFilesPreservesWhitespaceAndNewlinesFromNULTerminatedGitPaths(t *testing.T) {
	want := []string{" leading.txt", "line\nbreak.txt", "trailing .txt"}
	got, err := parseNULTerminatedGitPaths([]byte("trailing .txt\x00line\nbreak.txt\x00 leading.txt\x00"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("baseline paths = %#v, want %#v", got, want)
	}
}
func TestInitialBaselineRejectsCredentialMaterialAndPlanDrift(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	git(t, "", "init", "-b", "main", repo)
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("GITHUB_TOKEN=ghp_abcdefghijklmnopqrstuvwxyz1234567890\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	files, err := BaselineFiles(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if findings := ScanCredentialMaterial(repo, files); len(findings) == 0 {
		t.Fatal("credential material not detected")
	}
	if _, err := CreateInitialBaseline(context.Background(), repo, "main", []BaselineFile{{Path: "different.txt", SHA256: strings.Repeat("a", 64)}}, "baseline"); err == nil {
		t.Fatal("approved file drift accepted")
	}
}

func TestInitialBaselineBindsAndRechecksExactFileBytes(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	git(t, "", "init", "-b", "main", repo)
	path := filepath.Join(repo, "README.md")
	approvedBytes := []byte("approved\n")
	if err := os.WriteFile(path, approvedBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := BaselineSnapshot(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(approvedBytes)
	if len(snapshot) != 1 || snapshot[0].Path != "README.md" || snapshot[0].SHA256 != hex.EncodeToString(want[:]) {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if err := os.WriteFile(path, []byte("replacement\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateInitialBaseline(context.Background(), repo, "main", snapshot, "baseline"); err == nil || !strings.Contains(err.Error(), "content drifted") {
		t.Fatalf("replacement bytes accepted: %v", err)
	}
}

func TestInitialBaselineBindsGitModeFromSnapshotThroughApply(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	git(t, "", "init", "-b", "main", repo)
	path := filepath.Join(repo, "script.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	blob := testGitOutput(t, repo, "hash-object", "-w", "script.sh")
	git(t, repo, "update-index", "--add", "--cacheinfo", "100755,"+blob+",script.sh")
	snapshot, err := BaselineSnapshot(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot) != 1 || snapshot[0].Mode != "100755" {
		t.Fatalf("baseline mode snapshot = %#v", snapshot)
	}
	git(t, repo, "update-index", "--cacheinfo", "100644,"+blob+",script.sh")
	if _, err := CreateInitialBaseline(context.Background(), repo, "main", snapshot, "baseline"); err == nil || !strings.Contains(err.Error(), "mode drifted") {
		t.Fatalf("git mode replacement accepted: %v", err)
	}
	git(t, repo, "update-index", "--cacheinfo", "100755,"+blob+",script.sh")
	commit, err := CreateInitialBaseline(context.Background(), repo, "main", snapshot, "baseline")
	if err != nil {
		t.Fatal(err)
	}
	if mode := strings.Fields(testGitOutput(t, repo, "ls-tree", commit, "--", "script.sh"))[0]; mode != "100755" {
		t.Fatalf("baseline tree mode = %q", mode)
	}
}

func TestInitialBaselineBindsSymlinkModeAndTargetBlob(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	git(t, "", "init", "-b", "main", repo)
	path := filepath.Join(repo, "current")
	if err := os.WriteFile(path, []byte("target-a"), 0o600); err != nil {
		t.Fatal(err)
	}
	blob := testGitOutput(t, repo, "hash-object", "-w", "current")
	git(t, repo, "update-index", "--add", "--cacheinfo", "120000,"+blob+",current")
	snapshot, err := BaselineSnapshot(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot) != 1 || snapshot[0].Mode != "120000" {
		t.Fatalf("symlink snapshot = %#v", snapshot)
	}
	if err := os.WriteFile(path, []byte("target-b"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateInitialBaseline(context.Background(), repo, "main", snapshot, "baseline"); err == nil || !strings.Contains(err.Error(), "content drifted") {
		t.Fatalf("symlink target replacement accepted: %v", err)
	}
	if err := os.WriteFile(path, []byte("target-a"), 0o600); err != nil {
		t.Fatal(err)
	}
	commit, err := CreateInitialBaseline(context.Background(), repo, "main", snapshot, "baseline")
	if err != nil {
		t.Fatal(err)
	}
	if entry := testGitOutput(t, repo, "ls-tree", commit, "--", "current"); !strings.HasPrefix(entry, "120000 ") {
		t.Fatalf("symlink tree entry = %q", entry)
	}
	if target := testGitOutput(t, repo, "show", commit+":current"); target != "target-a" {
		t.Fatalf("symlink blob = %q", target)
	}
}

func TestInitialBaselineGitPlumbingIsDeterministicUnderHostileGitEnvironment(t *testing.T) {
	create := func() string {
		repo := filepath.Join(t.TempDir(), "repo")
		git(t, "", "init", "-b", "main", repo)
		if err := os.WriteFile(filepath.Join(repo, "approved.txt"), []byte("approved bytes\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return repo
	}
	firstRepo := create()
	firstSnapshot, err := BaselineSnapshot(context.Background(), firstRepo)
	if err != nil {
		t.Fatal(err)
	}
	first, err := CreateInitialBaseline(context.Background(), firstRepo, "main", firstSnapshot, "Initial repository baseline")
	if err != nil {
		t.Fatal(err)
	}

	secondRepo := create()
	globalHooks := filepath.Join(t.TempDir(), "hooks")
	if err := os.Mkdir(globalHooks, 0o700); err != nil {
		t.Fatal(err)
	}
	hookCapture := filepath.Join(t.TempDir(), "reference-hook-ran")
	if err := os.WriteFile(filepath.Join(globalHooks, "reference-transaction"), []byte("#!/bin/sh\nprintf invoked > \"$WORKFLOW_BASELINE_HOOK_CAPTURE\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	globalConfig := filepath.Join(t.TempDir(), "hostile.gitconfig")
	config := "[user]\n\tname = Hostile\n\temail = hostile@example.invalid\n[core]\n\thooksPath = " + filepath.ToSlash(globalHooks) + "\n[commit]\n\tgpgSign = true\n[i18n]\n\tcommitEncoding = ISO-8859-1\n"
	if err := os.WriteFile(globalConfig, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalConfig)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "0")
	t.Setenv("GIT_INDEX_FILE", filepath.Join(t.TempDir(), "hostile-index"))
	t.Setenv("GIT_OBJECT_DIRECTORY", filepath.Join(t.TempDir(), "hostile-objects"))
	t.Setenv("GIT_AUTHOR_NAME", "Hostile Environment")
	t.Setenv("GIT_AUTHOR_DATE", "2037-01-01T00:00:00Z")
	t.Setenv("GIT_COMMITTER_DATE", "2038-01-01T00:00:00Z")
	t.Setenv("WORKFLOW_BASELINE_HOOK_CAPTURE", hookCapture)
	secondSnapshot, err := BaselineSnapshot(context.Background(), secondRepo)
	if err != nil {
		t.Fatalf("snapshot inherited hostile Git environment: %v", err)
	}
	second, err := CreateInitialBaseline(context.Background(), secondRepo, "main", secondSnapshot, "Initial repository baseline")
	if err != nil {
		t.Fatalf("baseline inherited hostile Git environment: %v", err)
	}
	if first != second {
		t.Fatalf("same approved tree/message produced %s then %s", first, second)
	}
	if _, err := os.Stat(hookCapture); !os.IsNotExist(err) {
		t.Fatalf("baseline executed hostile Git hook: %v", err)
	}
	for _, repo := range []string{firstRepo, secondRepo} {
		command := exec.Command("git", "-C", repo, "show-ref", "--head")
		command.Env = isolatedGitEnvironment(nil)
		output, err := command.Output()
		if err != nil || string(output) != first+" HEAD\n"+first+" refs/heads/main\n" {
			t.Fatalf("baseline refs for %s = %q, %v", repo, output, err)
		}
	}
}

func TestInitialBaselineHonorsAndBindsGlobalExcludesFile(t *testing.T) {
	home := t.TempDir()
	excludes := filepath.Join(home, "workflow-global-excludes")
	if err := os.WriteFile(excludes, []byte("*.secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[core]\n\texcludesFile = "+filepath.ToSlash(excludes)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))

	repo := filepath.Join(t.TempDir(), "repo")
	git(t, "", "init", "-b", "main", repo)
	for name, contents := range map[string]string{"visible.txt": "approved\n", "omitted.secret": "ignored\n"} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := BaselineSnapshot(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot) != 1 || snapshot[0].Path != "visible.txt" {
		t.Fatalf("global excludes semantics not preserved: %#v", snapshot)
	}
	discovery, err := Discover(context.Background(), repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	approved, err := CaptureApprovalSnapshot(context.Background(), discovery, "owner/repo", snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var binding ApprovalSnapshot
	if err := json.Unmarshal([]byte(approved), &binding); err != nil {
		t.Fatal(err)
	}
	if filepath.Clean(binding.GlobalExcludesPath) != filepath.Clean(excludes) || binding.GlobalExcludesSHA256 == "" {
		t.Fatalf("global excludes binding = %#v", binding)
	}
	if err := os.WriteFile(excludes, []byte("*.secret\n# approval-time semantics changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyApprovalSnapshot(context.Background(), approved); err == nil || !strings.Contains(err.Error(), "global excludes") {
		t.Fatalf("global excludes drift accepted: %v", err)
	}
}

func TestInitialBaselineHonorsImplicitGlobalExcludesFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	excludes := filepath.Join(home, ".config", "git", "ignore")
	if err := os.MkdirAll(filepath.Dir(excludes), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(excludes, []byte("*.secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(t.TempDir(), "repo")
	git(t, "", "init", "-b", "main", repo)
	for name, contents := range map[string]string{"visible.txt": "approved\n", "omitted.secret": "ignored\n"} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := BaselineSnapshot(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot) != 1 || snapshot[0].Path != "visible.txt" {
		t.Fatalf("implicit global excludes semantics not preserved: %#v", snapshot)
	}
	binding, err := resolveGlobalExcludes(context.Background(), repo)
	if err != nil || filepath.Clean(binding.Path) != filepath.Clean(excludes) || binding.SHA256 == "" {
		t.Fatalf("implicit binding=%#v err=%v", binding, err)
	}
}

func TestInitialBaselineHonorsRepositoryLocalExcludesFilePrecedence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	global := filepath.Join(home, "global-ignore")
	if err := os.WriteFile(global, []byte("*.global\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[core]\n\texcludesFile = "+filepath.ToSlash(global)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(t.TempDir(), "repo")
	git(t, "", "init", "-b", "main", repo)
	local := filepath.Join(repo, "local-ignore")
	if err := os.WriteFile(local, []byte("*.local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "config", "core.excludesFile", "local-ignore")
	for name := range map[string]bool{"visible.txt": true, "omitted.local": true, "kept.global": true} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	files, err := BaselineFiles(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(files, ",") != "kept.global,local-ignore,visible.txt" {
		t.Fatalf("local excludes precedence files=%#v", files)
	}
	binding, err := resolveGlobalExcludes(context.Background(), repo)
	if err != nil || filepath.Clean(binding.Path) != filepath.Clean(local) {
		t.Fatalf("local binding=%#v err=%v", binding, err)
	}
}
