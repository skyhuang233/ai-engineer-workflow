package github

import (
	"context"
	"fmt"
	"net/url"
	"strconv"

	"github.com/skyhuang233/workflow/internal/store"
)

func (c *Client) PullRequestChecks(ctx context.Context, repository, commit string) ([]store.PullRequestCheck, error) {
	if err := ValidateRepository(repository); err != nil {
		return nil, err
	}
	if commit == "" {
		return nil, fmt.Errorf("candidate commit is required")
	}
	type checkRun struct {
		ID         int64  `json:"id"`
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		HeadSHA    string `json:"head_sha"`
	}
	type response struct {
		CheckRuns []checkRun `json:"check_runs"`
	}
	var checks []store.PullRequestCheck
	for page := 1; ; page++ {
		var result response
		path := "/repos/" + repository + "/commits/" + url.PathEscape(commit) + "/check-runs?per_page=100&page=" + strconv.Itoa(page)
		if err := c.getJSON(ctx, path, &result); err != nil {
			return nil, err
		}
		for _, check := range result.CheckRuns {
			checks = append(checks, store.PullRequestCheck{CheckRunID: check.ID, Name: check.Name, Status: check.Status, Conclusion: check.Conclusion, HeadSHA: check.HeadSHA})
		}
		if len(result.CheckRuns) < 100 {
			return checks, nil
		}
	}
}
