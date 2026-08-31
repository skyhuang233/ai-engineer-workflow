package controlplane

import (
	"context"
	"errors"
	"testing"
)

type ghExecutorFunc func(context.Context, ...string) ([]byte, error)

func (f ghExecutorFunc) Run(ctx context.Context, args ...string) ([]byte, error) {
	return f(ctx, args...)
}

func TestGitHubCLIIssueObserverReturnsOnlyNewNonPullRequestIssues(t *testing.T) {
	observer := GitHubCLIIssueObserver{Executor: ghExecutorFunc(func(_ context.Context, args ...string) ([]byte, error) {
		if len(args) != 4 || args[0] != "api" || args[1] != "--paginate" || args[2] != "--slurp" {
			t.Fatalf("args=%q", args)
		}
		return []byte(`[[{"id":8,"number":1,"title":"old","body":"","state":"open","created_at":"2026-08-31T00:00:00Z","updated_at":"2026-08-31T00:00:00Z"}],[{"id":9,"number":2,"title":"pull","body":"","state":"open","created_at":"2026-08-31T00:00:00Z","updated_at":"2026-08-31T00:00:00Z","pull_request":{}},{"id":10,"number":3,"title":"new","body":"body","state":"closed","created_at":"2026-08-31T00:00:01Z","updated_at":"2026-08-31T00:00:02Z"}]]`), nil
	})}
	issues, err := observer.IssuesAfter(context.Background(), "owner/repository", 8)
	if err != nil || len(issues) != 1 || issues[0].ID != 10 || issues[0].Title != "new" {
		t.Fatalf("issues=%+v err=%v", issues, err)
	}
}

func TestGitHubCLIIssueObserverIncludesCommandOutputInFailure(t *testing.T) {
	observer := GitHubCLIIssueObserver{Executor: ghExecutorFunc(func(context.Context, ...string) ([]byte, error) { return []byte("not logged in"), errors.New("exit") })}
	_, err := observer.IssuesAfter(context.Background(), "owner/repository", 0)
	if err == nil || !contains(err.Error(), "not logged in") {
		t.Fatalf("error=%v", err)
	}
}

func contains(value, part string) bool {
	for index := 0; index+len(part) <= len(value); index++ {
		if value[index:index+len(part)] == part {
			return true
		}
	}
	return false
}
