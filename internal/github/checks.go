package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/skyhuang233/workflow/internal/store"
)

func (c *Client) PullRequestChecks(ctx context.Context, repository, commit string) ([]store.PullRequestCheck, error) {
	checks, _, _, err := c.PullRequestChecksIfChanged(ctx, repository, commit, "", true)
	return checks, err
}

func (c *Client) PullRequestChecksIfChanged(ctx context.Context, repository, commit, etag string, full bool) ([]store.PullRequestCheck, string, bool, error) {
	if err := ValidateRepository(repository); err != nil {
		return nil, "", false, err
	}
	if commit == "" {
		return nil, "", false, fmt.Errorf("candidate commit is required")
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
		if page == 1 {
			changed, responseETag, err := c.getJSONIfChanged(ctx, path, &result, etag, full)
			if err != nil {
				return nil, "", false, err
			}
			if !changed {
				return nil, etag, false, nil
			}
			etag = responseETag
		} else if err := c.getJSON(ctx, path, &result); err != nil {
			return nil, "", false, err
		}
		for _, check := range result.CheckRuns {
			checks = append(checks, store.PullRequestCheck{CheckRunID: check.ID, Name: check.Name, Status: check.Status, Conclusion: check.Conclusion, HeadSHA: check.HeadSHA})
		}
		if len(result.CheckRuns) < 100 {
			return checks, etag, true, nil
		}
	}
}

func (c *Client) getJSONIfChanged(ctx context.Context, path string, destination any, etag string, full bool) (bool, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return false, "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if !full && etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	response, err := c.HTTP.Do(req)
	if err != nil {
		return false, "", err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotModified {
		return false, etag, nil
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return false, "", &apiError{Method: http.MethodGet, Path: path, StatusCode: response.StatusCode, Message: response.Status, RetryAt: rateLimitRetryAt(response, time.Now().UTC())}
	}
	if err := json.NewDecoder(response.Body).Decode(destination); err != nil {
		return false, "", err
	}
	return true, response.Header.Get("ETag"), nil
}
