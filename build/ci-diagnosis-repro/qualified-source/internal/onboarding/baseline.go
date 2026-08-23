package onboarding

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var credentialPattern = regexp.MustCompile(`(?i)(github[_-]?token|password|secret|private[_-]?key)\s*[:=]|ghp_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|-----BEGIN [A-Z ]*PRIVATE KEY-----`)
var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type BaselineFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Mode   string `json:"mode"`
}

func BaselineFiles(ctx context.Context, repository string) ([]string, error) {
	if err := ValidateLocalGitReadConfiguration(ctx, repository); err != nil {
		return nil, err
	}
	binding, err := resolveGlobalExcludes(ctx, repository)
	if err != nil {
		return nil, err
	}
	args := []string{"ls-files", "-z", "--cached", "--others", "--exclude-standard"}
	prefix := baselineGitArgs()
	prefix = append(prefix, "-c", "core.excludesFile="+binding.Path)
	command := exec.CommandContext(ctx, "git", append(prefix, args...)...)
	command.Dir = repository
	command.Env = isolatedGitEnvironment(nil)
	output, err := command.Output()
	if err != nil {
		return nil, err
	}
	return parseNULTerminatedGitPaths(output)
}

type globalExcludesBinding struct {
	Path, SHA256 string
}

func resolveGlobalExcludes(ctx context.Context, repository string) (globalExcludesBinding, error) {
	scopes := []string{"local", "global", "system"}
	worktreeConfig, err := repositoryWorktreeConfigEnabled(ctx, repository)
	if err != nil {
		return globalExcludesBinding{}, err
	}
	if worktreeConfig {
		scopes = append([]string{"worktree"}, scopes...)
	}
	for _, scope := range scopes {
		paths, err := scopedExcludesPaths(ctx, repository, scope)
		if err != nil {
			return globalExcludesBinding{}, err
		}
		if len(paths) == 0 {
			continue
		}
		if len(paths) != 1 || strings.TrimSpace(paths[0]) == "" {
			return globalExcludesBinding{}, fmt.Errorf("%s Git scope has an unsupported core.excludesFile definition", scope)
		}
		path := paths[0]
		if !filepath.IsAbs(path) {
			base := repository
			if scope == "global" {
				base = globalGitHome()
			}
			if scope == "system" || base == "" {
				return globalExcludesBinding{}, fmt.Errorf("relative %s core.excludesFile is unsupported", scope)
			}
			path, err = filepath.Abs(filepath.Join(base, path))
			if err != nil {
				return globalExcludesBinding{}, err
			}
		}
		return bindGlobalExcludes(path)
	}
	path := defaultGlobalExcludesPath()
	if path == "" {
		return globalExcludesBinding{}, nil
	}
	return bindGlobalExcludes(path)
}

func scopedExcludesPaths(ctx context.Context, repository, scope string) ([]string, error) {
	includeMode := "--no-includes"
	if scope == "global" || scope == "system" {
		includeMode = "--includes"
	}
	command := exec.CommandContext(ctx, "git", "config", "--"+scope, includeMode, "--path", "--get-all", "core.excludesFile")
	command.Dir = repository
	if scope == "local" || scope == "worktree" {
		command.Env = isolatedGitEnvironment([]string{"GIT_OPTIONAL_LOCKS=0"})
	} else {
		command.Env = hostScopedGitEnvironment(scope == "system")
	}
	output, err := command.Output()
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == 1 {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s Git core.excludesFile: %w", scope, err)
	}
	text := strings.TrimRight(string(output), "\r\n")
	if text == "" {
		return []string{""}, nil
	}
	return strings.FieldsFunc(text, func(r rune) bool { return r == '\r' || r == '\n' }), nil
}

func bindGlobalExcludes(path string) (globalExcludesBinding, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			sum := sha256.Sum256([]byte("absent"))
			return globalExcludesBinding{Path: filepath.Clean(path), SHA256: hex.EncodeToString(sum[:])}, nil
		}
		return globalExcludesBinding{}, fmt.Errorf("read global Git excludesFile %q: %w", path, err)
	}
	sum := sha256.Sum256(data)
	return globalExcludesBinding{Path: filepath.Clean(path), SHA256: hex.EncodeToString(sum[:])}, nil
}

func globalGitHome() string {
	if value := strings.TrimSpace(os.Getenv("HOME")); value != "" {
		return value
	}
	return strings.TrimSpace(os.Getenv("USERPROFILE"))
}

func defaultGlobalExcludesPath() string {
	if root := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); root != "" {
		return filepath.Join(root, "git", "ignore")
	}
	if home := globalGitHome(); home != "" {
		return filepath.Join(home, ".config", "git", "ignore")
	}
	return ""
}

func hostGlobalGitEnvironment() []string {
	return hostScopedGitEnvironment(false)
}

func hostScopedGitEnvironment(allowSystem bool) []string {
	environment := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(strings.ToUpper(key), "GIT_") {
			continue
		}
		environment = append(environment, entry)
	}
	if !allowSystem {
		environment = append(environment, "GIT_CONFIG_NOSYSTEM=1")
	}
	return append(environment, "GIT_OPTIONAL_LOCKS=0")
}

func parseNULTerminatedGitPaths(output []byte) ([]string, error) {
	if len(output) == 0 {
		return []string{}, nil
	}
	if output[len(output)-1] != 0 {
		return nil, errors.New("Git path list is not NUL terminated")
	}
	seen := map[string]struct{}{}
	for _, raw := range bytes.Split(output[:len(output)-1], []byte{0}) {
		path := string(raw)
		if path == "" {
			return nil, errors.New("Git path list contains an empty record")
		}
		if !strings.HasPrefix(path, ".git/") {
			seen[path] = struct{}{}
		}
	}
	files := make([]string, 0, len(seen))
	for path := range seen {
		files = append(files, path)
	}
	sort.Strings(files)
	return files, nil
}

// BaselineSnapshot binds every approved baseline path to the exact bytes Git
// will place at that path. Symlinks bind their link target bytes, matching the
// blob representation used by Git.
func BaselineSnapshot(ctx context.Context, repository string) ([]BaselineFile, error) {
	files, err := BaselineFiles(ctx, repository)
	if err != nil {
		return nil, err
	}
	result := make([]BaselineFile, 0, len(files))
	for _, relative := range files {
		data, err := baselineWorkingBytes(repository, relative)
		if err != nil {
			return nil, fmt.Errorf("read Initial Repository Baseline path %q: %w", relative, err)
		}
		sum := sha256.Sum256(data)
		mode, err := baselineGitMode(ctx, repository, relative)
		if err != nil {
			return nil, fmt.Errorf("read Initial Repository Baseline mode %q: %w", relative, err)
		}
		result = append(result, BaselineFile{Path: relative, SHA256: hex.EncodeToString(sum[:]), Mode: mode})
	}
	return result, nil
}

func baselineGitMode(ctx context.Context, repository, relative string) (string, error) {
	staged, err := baselineGitBytes(ctx, repository, "ls-files", "--stage", "--", relative)
	if err != nil {
		return "", err
	}
	if fields := strings.Fields(string(staged)); len(fields) > 0 {
		return fields[0], nil
	}
	info, err := os.Lstat(filepath.Join(repository, filepath.FromSlash(relative)))
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "120000", nil
	}
	if info.Mode()&0o111 != 0 {
		return "100755", nil
	}
	return "100644", nil
}

func baselineWorkingBytes(repository, relative string) ([]byte, error) {
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(relative)))
	if clean != relative || clean == "." || filepath.IsAbs(relative) || strings.HasPrefix(clean, "../") {
		return nil, errors.New("baseline path is not repository-relative")
	}
	path := filepath.Join(repository, filepath.FromSlash(relative))
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		return []byte(filepath.ToSlash(target)), err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("baseline path is not a regular file or symlink")
	}
	return os.ReadFile(path)
}

func verifyBaselineSnapshot(ctx context.Context, repository string, approved []BaselineFile) error {
	seen := map[string]bool{}
	for _, file := range approved {
		if seen[file.Path] || !sha256Pattern.MatchString(file.SHA256) || !validBaselineMode(file.Mode) {
			return errors.New("Initial Repository Baseline snapshot is invalid")
		}
		seen[file.Path] = true
		data, err := baselineWorkingBytes(repository, file.Path)
		if err != nil {
			return fmt.Errorf("Initial Repository Baseline path drifted: %s", file.Path)
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != file.SHA256 {
			return fmt.Errorf("Initial Repository Baseline content drifted: %s", file.Path)
		}
		mode, err := baselineGitMode(ctx, repository, file.Path)
		if err != nil || mode != file.Mode {
			return fmt.Errorf("Initial Repository Baseline mode drifted: %s", file.Path)
		}
	}
	return nil
}

func validBaselineMode(mode string) bool {
	return mode == "100644" || mode == "100755" || mode == "120000"
}

func VerifyInitialBaseline(ctx context.Context, repository, commit string, approved []BaselineFile) error {
	if !fullSHA.MatchString(commit) {
		return errors.New("Initial Repository Baseline commit is invalid")
	}
	paths, err := baselineGitBytes(ctx, repository, "ls-tree", "-r", "-z", "--name-only", commit)
	if err != nil {
		return err
	}
	wantPaths := make([]string, 0, len(approved))
	for _, file := range approved {
		wantPaths = append(wantPaths, file.Path)
	}
	sort.Strings(wantPaths)
	gotPaths, err := parseNULTerminatedGitPaths(paths)
	if err != nil {
		return err
	}
	if strings.Join(gotPaths, "\x00") != strings.Join(wantPaths, "\x00") {
		return errors.New("Initial Repository Baseline tree paths differ")
	}
	for _, file := range approved {
		data, err := baselineGitBytes(ctx, repository, "show", commit+":"+file.Path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != file.SHA256 {
			return fmt.Errorf("Initial Repository Baseline tree content differs: %s", file.Path)
		}
		treeEntry, err := baselineGitBytes(ctx, repository, "ls-tree", commit, "--", file.Path)
		if err != nil {
			return err
		}
		fields := strings.Fields(string(treeEntry))
		if len(fields) == 0 || !validBaselineMode(file.Mode) || fields[0] != file.Mode {
			return fmt.Errorf("Initial Repository Baseline tree mode differs: %s", file.Path)
		}
	}
	return nil
}
func ScanCredentialMaterial(repository string, files []string) []string {
	var findings []string
	for _, relative := range files {
		data, err := baselineWorkingBytes(repository, relative)
		if err != nil || len(data) > 2<<20 {
			continue
		}
		scanner := bufio.NewScanner(bytes.NewReader(data))
		line := 0
		for scanner.Scan() {
			line++
			if credentialPattern.MatchString(scanner.Text()) {
				findings = append(findings, fmt.Sprintf("%s:%d", relative, line))
				break
			}
		}
	}
	return findings
}

func CreateInitialBaseline(ctx context.Context, repository, branch string, approvedFiles []BaselineFile, message string) (string, error) {
	return CreateInitialBaselineWithBootstrap(ctx, repository, branch, approvedFiles, nil, message)
}

func CreateInitialBaselineWithBootstrap(ctx context.Context, repository, branch string, approvedFiles []BaselineFile, bootstrap map[string][]byte, message string) (string, error) {
	if _, err := baselineGitOutput(ctx, repository, "rev-parse", "--verify", "HEAD"); err == nil {
		return "", errors.New("Initial Repository Baseline requires zero commits")
	}
	current, err := BaselineFiles(ctx, repository)
	if err != nil {
		return "", err
	}
	approved := append([]BaselineFile(nil), approvedFiles...)
	sort.Slice(approved, func(i, j int) bool { return approved[i].Path < approved[j].Path })
	approvedPaths := make([]string, 0, len(approved))
	workspacePaths := make([]string, 0, len(approved))
	approvedBootstrap := 0
	for _, file := range approved {
		approvedPaths = append(approvedPaths, file.Path)
		data, generated := bootstrap[file.Path]
		if generated {
			approvedBootstrap++
			if file.Mode != "100644" {
				return "", fmt.Errorf("Initial Repository Baseline bootstrap mode is invalid: %s", file.Path)
			}
			sum := sha256.Sum256(data)
			if hex.EncodeToString(sum[:]) != file.SHA256 {
				return "", fmt.Errorf("Initial Repository Baseline bootstrap content differs: %s", file.Path)
			}
		} else {
			workspacePaths = append(workspacePaths, file.Path)
		}
	}
	if approvedBootstrap != len(bootstrap) {
		return "", errors.New("Initial Repository Baseline bootstrap files differ from the approved plan")
	}
	if strings.Join(current, "\x00") != strings.Join(workspacePaths, "\x00") {
		return "", errors.New("Initial Repository Baseline file list drifted from the approved plan")
	}
	workspaceApproved := make([]BaselineFile, 0, len(workspacePaths))
	for _, file := range approved {
		if _, generated := bootstrap[file.Path]; !generated {
			workspaceApproved = append(workspaceApproved, file)
		}
	}
	if err := verifyBaselineSnapshot(ctx, repository, workspaceApproved); err != nil {
		return "", err
	}
	if findings := ScanCredentialMaterial(repository, workspacePaths); len(findings) > 0 {
		return "", fmt.Errorf("credential material blocks baseline: %s", strings.Join(findings, ", "))
	}
	gitDir, err := baselineGitOutput(ctx, repository, "rev-parse", "--git-dir")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(repository, gitDir)
	}
	temporary, err := os.CreateTemp(filepath.Dir(gitDir), "workflow-baseline-index-*.tmp")
	if err != nil {
		return "", err
	}
	indexPath := temporary.Name()
	temporary.Close()
	os.Remove(indexPath)
	defer os.Remove(indexPath)
	run := func(input string, args ...string) (string, error) {
		command := exec.CommandContext(ctx, "git", baselineGitArgs(args...)...)
		command.Dir = repository
		command.Env = isolatedGitEnvironment([]string{"GIT_INDEX_FILE=" + indexPath})
		if input != "" {
			command.Stdin = strings.NewReader(input)
		}
		output, err := command.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
		}
		return strings.TrimSpace(string(output)), nil
	}
	runBytes := func(args ...string) ([]byte, error) {
		command := exec.CommandContext(ctx, "git", baselineGitArgs(args...)...)
		command.Dir = repository
		command.Env = isolatedGitEnvironment([]string{"GIT_INDEX_FILE=" + indexPath})
		output, err := command.Output()
		if err != nil {
			return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
		}
		return output, nil
	}
	if _, err := run("", "read-tree", "--empty"); err != nil {
		return "", err
	}
	if len(approvedPaths) > 0 {
		for _, file := range approved {
			data, generated := bootstrap[file.Path]
			if !generated {
				data, err = baselineWorkingBytes(repository, file.Path)
				if err != nil {
					return "", err
				}
			}
			sum := sha256.Sum256(data)
			if hex.EncodeToString(sum[:]) != file.SHA256 {
				return "", fmt.Errorf("Initial Repository Baseline content drifted: %s", file.Path)
			}
			blob, err := run(string(data), "hash-object", "-w", "--stdin")
			if err != nil {
				return "", err
			}
			if _, err := run("", "update-index", "--add", "--cacheinfo", file.Mode, blob, file.Path); err != nil {
				return "", err
			}
		}
		// Re-read the staged blobs so a replacement racing with git add cannot
		// silently enter the approved baseline.
		for _, file := range approved {
			data, err := runBytes("show", ":"+file.Path)
			if err != nil {
				return "", err
			}
			sum := sha256.Sum256(data)
			if hex.EncodeToString(sum[:]) != file.SHA256 {
				return "", fmt.Errorf("Initial Repository Baseline staged content drifted: %s", file.Path)
			}
			entry, err := run("", "ls-files", "--stage", "--", file.Path)
			if err != nil {
				return "", err
			}
			fields := strings.Fields(entry)
			if len(fields) == 0 || fields[0] != file.Mode {
				return "", fmt.Errorf("Initial Repository Baseline staged mode drifted: %s", file.Path)
			}
		}
	}
	tree, err := run("", "write-tree")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(message) == "" {
		message = "Initial Repository Baseline"
	}
	commitEnvironment := []string{
		"GIT_INDEX_FILE=" + indexPath,
		"GIT_AUTHOR_NAME=Agent Workflow Setup",
		"GIT_AUTHOR_EMAIL=workflow@localhost",
		"GIT_AUTHOR_DATE=2000-01-01T00:00:00Z",
		"GIT_COMMITTER_NAME=Agent Workflow Setup",
		"GIT_COMMITTER_EMAIL=workflow@localhost",
		"GIT_COMMITTER_DATE=2000-01-01T00:00:00Z",
	}
	commitCommand := exec.CommandContext(ctx, "git", baselineGitArgs("commit-tree", tree, "-F", "-")...)
	commitCommand.Dir = repository
	commitCommand.Env = isolatedGitEnvironment(commitEnvironment)
	commitCommand.Stdin = strings.NewReader(message)
	commitOutput, commitErr := commitCommand.CombinedOutput()
	if commitErr != nil {
		return "", fmt.Errorf("git commit-tree: %w (%s)", commitErr, strings.TrimSpace(string(commitOutput)))
	}
	commit := strings.TrimSpace(string(commitOutput))
	if !fullSHA.MatchString(commit) {
		return "", errors.New("git commit-tree returned an invalid commit")
	}
	if _, err := baselineGitOutput(ctx, repository, "update-ref", "refs/heads/"+branch, commit, strings.Repeat("0", 40)); err != nil {
		return "", err
	}
	if _, err := baselineGitOutput(ctx, repository, "symbolic-ref", "HEAD", "refs/heads/"+branch); err != nil {
		return "", err
	}
	for relative, data := range bootstrap {
		target := filepath.Join(repository, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return "", err
		}
		if err := os.WriteFile(target, data, 0o600); err != nil {
			return "", err
		}
	}
	return commit, nil
}

func baselineGitArgs(args ...string) []string {
	prefix := []string{
		"-c", "core.autocrlf=false",
		"-c", "core.safecrlf=false",
		"-c", "core.hooksPath=" + os.DevNull,
		"-c", "core.attributesFile=" + os.DevNull,
		"-c", "core.fsmonitor=false",
		"-c", "commit.gpgSign=false",
		"-c", "tag.gpgSign=false",
		"-c", "user.name=Agent Workflow Setup",
		"-c", "user.email=workflow@localhost",
		"-c", "user.useConfigOnly=true",
		"-c", "i18n.commitEncoding=UTF-8",
		"-c", "i18n.logOutputEncoding=UTF-8",
		"-c", "credential.helper=",
		"-c", "credential.interactive=never",
	}
	return append(prefix, args...)
}

func baselineGitBytes(ctx context.Context, repository string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "git", baselineGitArgs(args...)...)
	command.Dir = repository
	command.Env = isolatedGitEnvironment(nil)
	output, err := command.Output()
	if err != nil {
		return nil, err
	}
	return output, nil
}

func baselineGitOutput(ctx context.Context, repository string, args ...string) (string, error) {
	output, err := baselineGitBytes(ctx, repository, args...)
	return strings.TrimSpace(string(output)), err
}
