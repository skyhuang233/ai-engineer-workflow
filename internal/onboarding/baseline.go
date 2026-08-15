package onboarding

import (
	"bufio"
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
}

func BaselineFiles(ctx context.Context, repository string) ([]string, error) {
	output, err := gitBytes(ctx, repository, "ls-files", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	for _, line := range strings.Split(string(output), "\n") {
		path := filepath.ToSlash(strings.TrimSpace(line))
		if path != "" && !strings.HasPrefix(path, ".git/") {
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
		result = append(result, BaselineFile{Path: relative, SHA256: hex.EncodeToString(sum[:])})
	}
	return result, nil
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

func verifyBaselineSnapshot(repository string, approved []BaselineFile) error {
	seen := map[string]bool{}
	for _, file := range approved {
		if seen[file.Path] || !sha256Pattern.MatchString(file.SHA256) {
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
	}
	return nil
}

func VerifyInitialBaseline(ctx context.Context, repository, commit string, approved []BaselineFile) error {
	if !fullSHA.MatchString(commit) {
		return errors.New("Initial Repository Baseline commit is invalid")
	}
	paths, err := gitBytes(ctx, repository, "ls-tree", "-r", "--name-only", commit)
	if err != nil {
		return err
	}
	wantPaths := make([]string, 0, len(approved))
	for _, file := range approved {
		wantPaths = append(wantPaths, file.Path)
	}
	sort.Strings(wantPaths)
	gotPaths := []string{}
	for _, path := range strings.Split(strings.TrimSuffix(strings.ReplaceAll(string(paths), "\r\n", "\n"), "\n"), "\n") {
		if path != "" {
			gotPaths = append(gotPaths, path)
		}
	}
	if strings.Join(gotPaths, "\x00") != strings.Join(wantPaths, "\x00") {
		return errors.New("Initial Repository Baseline tree paths differ")
	}
	for _, file := range approved {
		data, err := gitBytes(ctx, repository, "show", commit+":"+file.Path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != file.SHA256 {
			return fmt.Errorf("Initial Repository Baseline tree content differs: %s", file.Path)
		}
	}
	return nil
}
func ScanCredentialMaterial(repository string, files []string) []string {
	var findings []string
	for _, relative := range files {
		path := filepath.Join(repository, filepath.FromSlash(relative))
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() > 2<<20 {
			continue
		}
		file, err := os.Open(path)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(file)
		line := 0
		for scanner.Scan() {
			line++
			if credentialPattern.MatchString(scanner.Text()) {
				findings = append(findings, fmt.Sprintf("%s:%d", relative, line))
				break
			}
		}
		file.Close()
	}
	return findings
}

func CreateInitialBaseline(ctx context.Context, repository, branch string, approvedFiles []BaselineFile, message string) (string, error) {
	if _, err := gitOutput(ctx, repository, "rev-parse", "--verify", "HEAD"); err == nil {
		return "", errors.New("Initial Repository Baseline requires zero commits")
	}
	current, err := BaselineFiles(ctx, repository)
	if err != nil {
		return "", err
	}
	approved := append([]BaselineFile(nil), approvedFiles...)
	sort.Slice(approved, func(i, j int) bool { return approved[i].Path < approved[j].Path })
	approvedPaths := make([]string, 0, len(approved))
	for _, file := range approved {
		approvedPaths = append(approvedPaths, file.Path)
	}
	if strings.Join(current, "\x00") != strings.Join(approvedPaths, "\x00") {
		return "", errors.New("Initial Repository Baseline file list drifted from the approved plan")
	}
	if err := verifyBaselineSnapshot(repository, approved); err != nil {
		return "", err
	}
	if findings := ScanCredentialMaterial(repository, approvedPaths); len(findings) > 0 {
		return "", fmt.Errorf("credential material blocks baseline: %s", strings.Join(findings, ", "))
	}
	gitDir, err := gitOutput(ctx, repository, "rev-parse", "--git-dir")
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
		command := exec.CommandContext(ctx, "git", args...)
		command.Dir = repository
		command.Env = append(os.Environ(), "GIT_INDEX_FILE="+indexPath)
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
		command := exec.CommandContext(ctx, "git", args...)
		command.Dir = repository
		command.Env = append(os.Environ(), "GIT_INDEX_FILE="+indexPath)
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
			data, err := baselineWorkingBytes(repository, file.Path)
			if err != nil {
				return "", err
			}
			sum := sha256.Sum256(data)
			if hex.EncodeToString(sum[:]) != file.SHA256 {
				return "", fmt.Errorf("Initial Repository Baseline content drifted: %s", file.Path)
			}
			blob, err := run(string(data), "hash-object", "-w", "--stdin")
			if err != nil {
				return "", err
			}
			info, err := os.Lstat(filepath.Join(repository, filepath.FromSlash(file.Path)))
			if err != nil {
				return "", err
			}
			mode := "100644"
			if info.Mode()&os.ModeSymlink != 0 {
				mode = "120000"
			} else if info.Mode()&0o111 != 0 {
				mode = "100755"
			}
			if _, err := run("", "update-index", "--add", "--cacheinfo", mode, blob, file.Path); err != nil {
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
		}
	}
	tree, err := run("", "write-tree")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(message) == "" {
		message = "Initial Repository Baseline"
	}
	commit, err := run(message, "-c", "user.name=Agent Workflow Setup", "-c", "user.email=workflow@localhost", "commit-tree", tree, "-F", "-")
	if err != nil {
		return "", err
	}
	if !fullSHA.MatchString(commit) {
		return "", errors.New("git commit-tree returned an invalid commit")
	}
	if _, err := gitOutput(ctx, repository, "update-ref", "refs/heads/"+branch, commit, strings.Repeat("0", 40)); err != nil {
		return "", err
	}
	if _, err := gitOutput(ctx, repository, "symbolic-ref", "HEAD", "refs/heads/"+branch); err != nil {
		return "", err
	}
	return commit, nil
}
