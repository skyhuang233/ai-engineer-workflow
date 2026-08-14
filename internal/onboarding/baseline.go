package onboarding

import (
	"bufio"
	"context"
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

func CreateInitialBaseline(ctx context.Context, repository, branch string, approvedFiles []string, message string) (string, error) {
	if _, err := gitOutput(ctx, repository, "rev-parse", "--verify", "HEAD"); err == nil {
		return "", errors.New("Initial Repository Baseline requires zero commits")
	}
	current, err := BaselineFiles(ctx, repository)
	if err != nil {
		return "", err
	}
	approved := append([]string(nil), approvedFiles...)
	sort.Strings(approved)
	if strings.Join(current, "\x00") != strings.Join(approved, "\x00") {
		return "", errors.New("Initial Repository Baseline file list drifted from the approved plan")
	}
	if findings := ScanCredentialMaterial(repository, approved); len(findings) > 0 {
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
	if _, err := run("", "read-tree", "--empty"); err != nil {
		return "", err
	}
	if len(approved) > 0 {
		args := append([]string{"add", "--"}, approved...)
		if _, err := run("", args...); err != nil {
			return "", err
		}
	}
	tree, err := run("", "write-tree")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(message) == "" {
		message = "Initial Repository Baseline"
	}
	commit, err := run(message, "commit-tree", tree, "-F", "-")
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
