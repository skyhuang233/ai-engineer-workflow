package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/skyhuang233/workflow/internal/store"
)

type OnboardingCheck struct {
	Name, Status, Conclusion, HeadSHA string
	AppID                             int64
}

// OnboardingChecks returns the check-run App identity needed by Setup's
// approval fence. The general poll projection intentionally remains unchanged.
func (c *Client) OnboardingChecks(ctx context.Context, repository, commit string) ([]OnboardingCheck, error) {
	if err := ValidateRepository(repository); err != nil {
		return nil, err
	}
	if commit == "" {
		return nil, errors.New("candidate commit is required")
	}
	type checkRun struct {
		Name, Status, Conclusion string
		HeadSHA                  string `json:"head_sha"`
		App                      struct {
			ID int64 `json:"id"`
		} `json:"app"`
	}
	type response struct {
		CheckRuns []checkRun `json:"check_runs"`
	}
	var checks []OnboardingCheck
	for page := 1; ; page++ {
		var result response
		path := "/repos/" + repository + "/commits/" + url.PathEscape(commit) + "/check-runs?per_page=100&page=" + strconv.Itoa(page)
		if err := c.getJSON(ctx, path, &result); err != nil {
			return nil, err
		}
		for _, check := range result.CheckRuns {
			checks = append(checks, OnboardingCheck{Name: check.Name, Status: check.Status, Conclusion: check.Conclusion, HeadSHA: check.HeadSHA, AppID: check.App.ID})
		}
		if len(result.CheckRuns) < 100 {
			return checks, nil
		}
	}
}

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
		message, _ := io.ReadAll(io.LimitReader(response.Body, 16<<10))
		detail := strings.TrimSpace(string(message))
		if detail == "" {
			detail = response.Status
		}
		return false, "", &apiError{Method: http.MethodGet, Path: path, StatusCode: response.StatusCode, Message: detail, RetryAt: rateLimitRetryAt(response, detail, time.Now().UTC())}
	}
	if err := json.NewDecoder(response.Body).Decode(destination); err != nil {
		return false, "", err
	}
	return true, response.Header.Get("ETag"), nil
}
