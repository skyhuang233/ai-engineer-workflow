// Package onboarding discovers and applies the repository-owned Workflow contract.
package onboarding

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

const ManagedBlockStart = "<!-- agent-workflow:start -->"
const ManagedBlockEnd = "<!-- agent-workflow:end -->"

var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)
var githubPathSegment = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

type RemoteHead interface {
	Resolve(context.Context, string) (defaultBranch, head string, err error)
}
type StaticRemoteHead struct{ DefaultBranch, Head string }

func (s StaticRemoteHead) Resolve(context.Context, string) (string, string, error) {
	return s.DefaultBranch, s.Head, nil
}

type Discovery struct {
	Root, Branch, Head, Origin, Repository, DefaultBranch, RemoteHead string
	Published, HasCommits, Dirty                                      bool
}

func Discover(ctx context.Context, repository string, resolver RemoteHead) (Discovery, error) {
	root, err := gitOutput(ctx, repository, "rev-parse", "--show-toplevel")
	if err != nil {
		return Discovery{}, errors.New("current directory is not a Git repository")
	}
	root, _ = filepath.Abs(root)
	branch, err := gitOutput(ctx, root, "branch", "--show-current")
	if err != nil || branch == "" {
		return Discovery{}, errors.New("repository must have a checked-out branch")
	}
	head, headErr := gitOutput(ctx, root, "rev-parse", "--verify", "HEAD")
	hasCommits := headErr == nil
	origin, originErr := gitOutput(ctx, root, "remote", "get-url", "origin")
	published := originErr == nil && strings.TrimSpace(origin) != ""
	repositoryID := ""
	defaultBranch := branch
	remoteHead := ""
	if published {
		repositoryID, err = parseGitHubOrigin(origin)
		if err != nil {
			return Discovery{}, err
		}
		if resolver == nil {
			resolver = LSRemoteHead{}
		}
		transportURL, urlErr := GitHubHTTPSURL(repositoryID)
		if urlErr != nil {
			return Discovery{}, urlErr
		}
		defaultBranch, remoteHead, err = resolver.Resolve(ctx, transportURL)
		if err != nil {
			return Discovery{}, fmt.Errorf("read GitHub origin default branch without fetch: %w", err)
		}
		if branch != defaultBranch {
			return Discovery{}, fmt.Errorf("checked-out branch %q is not default branch %q", branch, defaultBranch)
		}
		if !hasCommits || head != remoteHead {
			return Discovery{}, errors.New("local default-branch HEAD must exactly equal GitHub origin default-branch HEAD")
		}
	}
	status, err := gitBytes(ctx, root, "status", "--porcelain=v2", "-z", "--untracked-files=all")
	if err != nil {
		return Discovery{}, err
	}
	dirty := len(status) != 0
	conflict, err := managedConflict(status)
	if err != nil {
		return Discovery{}, err
	}
	if conflict != "" {
		return Discovery{}, fmt.Errorf("local change overlaps Workflow-managed path %q", conflict)
	}
	if err := managedBlockConflict(ctx, root, hasCommits); err != nil {
		return Discovery{}, err
	}
	return Discovery{Root: root, Branch: branch, Head: head, Origin: origin, Repository: repositoryID, DefaultBranch: defaultBranch, RemoteHead: remoteHead, Published: published, HasCommits: hasCommits, Dirty: dirty}, nil
}

func managedConflict(status []byte) (string, error) {
	records := bytes.Split(status, []byte{0})
	for index := 0; index < len(records); index++ {
		record := string(records[index])
		if record == "" {
			continue
		}
		var paths []string
		switch record[0] {
		case '1':
			fields := strings.SplitN(record, " ", 9)
			if len(fields) != 9 {
				return "", errors.New("Git status returned a malformed ordinary entry")
			}
			paths = []string{fields[8]}
		case '2':
			fields := strings.SplitN(record, " ", 10)
			if len(fields) != 10 || index+1 >= len(records) || len(records[index+1]) == 0 {
				return "", errors.New("Git status returned a malformed rename entry")
			}
			index++
			paths = []string{fields[9], string(records[index])}
		case 'u':
			fields := strings.SplitN(record, " ", 11)
			if len(fields) != 11 {
				return "", errors.New("Git status returned a malformed unmerged entry")
			}
			paths = []string{fields[10]}
		case '?':
			if !strings.HasPrefix(record, "? ") {
				return "", errors.New("Git status returned a malformed untracked entry")
			}
			paths = []string{strings.TrimPrefix(record, "? ")}
		case '!', '#':
			continue
		default:
			return "", errors.New("Git status returned an unsupported porcelain-v2 entry")
		}
		for _, path := range paths {
			path = strings.ReplaceAll(path, `\`, "/")
			if path == "AGENTS.md" {
				continue
			}
			if path == ".github/workflows/workflow-contract.yml" || strings.HasPrefix(path, ".workflow/") || strings.HasPrefix(path, "docs/agents/") {
				return path, nil
			}
		}
	}
	return "", nil
}
func managedBlockConflict(ctx context.Context, root string, hasCommits bool) error {
	working, err := os.ReadFile(filepath.Join(root, "AGENTS.md"))
	if errors.Is(err, os.ErrNotExist) {
		working = nil
	} else if err != nil {
		return err
	}
	var base []byte
	if hasCommits {
		if output, showErr := gitBytes(ctx, root, "show", "HEAD:AGENTS.md"); showErr == nil {
			base = output
		}
	}
	workingBlock, wOK := extractManagedBlock(string(working))
	baseBlock, bOK := extractManagedBlock(string(base))
	if wOK != (bOK) || workingBlock != baseBlock {
		return errors.New("local AGENTS.md change overlaps the Workflow-managed block")
	}
	return nil
}
func extractManagedBlock(value string) (string, bool) {
	start := strings.Index(value, ManagedBlockStart)
	if start < 0 {
		return "", false
	}
	end := strings.Index(value[start:], ManagedBlockEnd)
	if end < 0 {
		return value[start:], true
	}
	end = start + end + len(ManagedBlockEnd)
	return value[start:end], true
}

func ParseGitHubOrigin(value string) (string, error) {
	if value != strings.TrimSpace(value) {
		return "", errors.New("origin must identify a canonical GitHub HTTPS or SSH repository")
	}
	if strings.HasPrefix(value, "git@github.com:") {
		path := strings.TrimSuffix(strings.TrimPrefix(value, "git@github.com:"), ".git")
		if validRepo(path) && (value == "git@github.com:"+path || value == "git@github.com:"+path+".git") {
			return path, nil
		}
	}
	parsed, err := url.Parse(value)
	if err == nil && parsed.Scheme == "https" && parsed.Host == "github.com" && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == "" && parsed.RawPath == "" {
		path := strings.TrimPrefix(parsed.Path, "/")
		path = strings.TrimSuffix(path, ".git")
		if validRepo(path) && (parsed.Path == "/"+path || parsed.Path == "/"+path+".git") {
			return path, nil
		}
	}
	return "", errors.New("origin must identify a canonical GitHub HTTPS or SSH repository")
}

func parseGitHubOrigin(value string) (string, error) { return ParseGitHubOrigin(value) }
func validRepo(value string) bool {
	parts := strings.Split(value, "/")
	if len(parts) != 2 {
		return false
	}
	for _, part := range parts {
		if !githubPathSegment.MatchString(part) || part == "." || part == ".." {
			return false
		}
	}
	return true
}

// GitHubHTTPSURL derives the credential-safe transport endpoint from a
// validated repository identity. Callers never need to rewrite an SSH origin.
func GitHubHTTPSURL(repository string) (string, error) {
	if !validRepo(repository) {
		return "", errors.New("GitHub repository identity is invalid")
	}
	return "https://github.com/" + repository + ".git", nil
}
func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	data, err := gitBytes(ctx, dir, args...)
	return strings.TrimSpace(string(data)), err
}
func gitBytes(ctx context.Context, dir string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = dir
	if len(args) > 0 && args[0] == "status" {
		command.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	}
	output, err := command.Output()
	if err != nil {
		return nil, err
	}
	return output, nil
}

type LSRemoteHead struct{}

func (LSRemoteHead) Resolve(ctx context.Context, origin string) (string, string, error) {
	command := exec.CommandContext(ctx, "git", "ls-remote", "--symref", origin, "HEAD")
	output, err := command.Output()
	if err != nil {
		return "", "", err
	}
	var branch, head string
	for _, line := range strings.Split(string(output), "\n") {
		if strings.HasPrefix(line, "ref: refs/heads/") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				branch = strings.TrimPrefix(fields[1], "refs/heads/")
			}
		} else {
			fields := strings.Fields(line)
			if len(fields) >= 2 && fields[1] == "HEAD" {
				head = fields[0]
			}
		}
	}
	if branch == "" || !fullSHA.MatchString(head) {
		return "", "", errors.New("origin HEAD is incomplete")
	}
	return branch, head, nil
}
