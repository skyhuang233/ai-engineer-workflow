package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

type GitHubCLIExecutor interface {
	Run(context.Context, ...string) ([]byte, error)
}
type OSGitHubCLIExecutor struct{}

func (OSGitHubCLIExecutor) Run(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "gh", args...).CombinedOutput()
}

// GitHubCLIIssueObserver derives its active GitHub CLI login at each call.
// It stores no token or CLI path; the process PATH is the current user’s
// ordinary environment as required by the native-service contract.
type GitHubCLIIssueObserver struct{ Executor GitHubCLIExecutor }

func (o GitHubCLIIssueObserver) IssuesAfter(ctx context.Context, repository string, cursor int64) ([]ObservedIssue, error) {
	if cursor < 0 {
		return nil, errors.New("Issue cursor must not be negative")
	}
	executor := o.Executor
	if executor == nil {
		executor = OSGitHubCLIExecutor{}
	}
	output, err := executor.Run(ctx, "api", "--paginate", "--slurp", "repos/"+repository+"/issues?state=all&sort=created&direction=asc&per_page=100")
	if err != nil {
		return nil, fmt.Errorf("list GitHub Issues: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	var pages [][]githubIssueResponse
	if err := json.Unmarshal(output, &pages); err != nil {
		return nil, fmt.Errorf("decode GitHub Issues: %w", err)
	}
	issues := make([]ObservedIssue, 0)
	for _, page := range pages {
		for _, item := range page {
			if item.PullRequest != nil || item.ID <= cursor {
				continue
			}
			created, err := time.Parse(time.RFC3339, item.CreatedAt)
			if err != nil {
				return nil, fmt.Errorf("decode GitHub Issue created_at: %w", err)
			}
			updated, err := time.Parse(time.RFC3339, item.UpdatedAt)
			if err != nil {
				return nil, fmt.Errorf("decode GitHub Issue updated_at: %w", err)
			}
			issues = append(issues, ObservedIssue{ID: item.ID, Number: item.Number, Title: item.Title, Body: item.Body, State: item.State, Created: created, Updated: updated})
		}
	}
	return issues, nil
}

type githubIssueResponse struct {
	ID          int64           `json:"id"`
	Number      int64           `json:"number"`
	Title       string          `json:"title"`
	Body        string          `json:"body"`
	State       string          `json:"state"`
	CreatedAt   string          `json:"created_at"`
	UpdatedAt   string          `json:"updated_at"`
	PullRequest json.RawMessage `json:"pull_request"`
}
