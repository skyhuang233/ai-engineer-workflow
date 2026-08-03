package github

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/skyhuang233/workflow/internal/plan"
)

const apiVersion = "2022-11-28"

type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

type PullRequestFeedback struct {
	Source  string
	EventID string
	Author  string
	Body    string
}

func (c *Client) ActionablePullRequestFeedback(ctx context.Context, repository string, number int64) ([]PullRequestFeedback, error) {
	if err := ValidateRepository(repository); err != nil {
		return nil, err
	}
	if number <= 0 {
		return nil, fmt.Errorf("pull request number is required")
	}
	type user struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	}
	type review struct {
		ID    int64  `json:"id"`
		Body  string `json:"body"`
		State string `json:"state"`
		User  user   `json:"user"`
	}
	type comment struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
		User user   `json:"user"`
	}
	var result []PullRequestFeedback
	for page := 1; ; page++ {
		var reviews []review
		path := "/repos/" + repository + "/pulls/" + strconv.FormatInt(number, 10) + "/reviews?per_page=100&page=" + strconv.Itoa(page)
		if err := c.getJSON(ctx, path, &reviews); err != nil {
			return nil, err
		}
		for _, value := range reviews {
			if value.State != "PENDING" && actionableHuman(value.User.Login, value.User.Type, value.Body) {
				result = append(result, PullRequestFeedback{Source: "review", EventID: strconv.FormatInt(value.ID, 10), Author: value.User.Login, Body: value.Body})
			}
		}
		if len(reviews) < 100 {
			break
		}
	}
	for _, endpoint := range []struct {
		source string
		path   string
	}{
		{source: "inline-comment", path: "/repos/" + repository + "/pulls/" + strconv.FormatInt(number, 10) + "/comments"},
		{source: "conversation-comment", path: "/repos/" + repository + "/issues/" + strconv.FormatInt(number, 10) + "/comments"},
	} {
		for page := 1; ; page++ {
			var comments []comment
			path := endpoint.path + "?per_page=100&page=" + strconv.Itoa(page)
			if err := c.getJSON(ctx, path, &comments); err != nil {
				return nil, err
			}
			for _, value := range comments {
				if actionableHuman(value.User.Login, value.User.Type, value.Body) {
					result = append(result, PullRequestFeedback{Source: endpoint.source, EventID: strconv.FormatInt(value.ID, 10), Author: value.User.Login, Body: value.Body})
				}
			}
			if len(comments) < 100 {
				break
			}
		}
	}
	return result, nil
}

func actionableHuman(login, accountType, body string) bool {
	if strings.TrimSpace(body) == "" || strings.EqualFold(accountType, "bot") || strings.HasSuffix(strings.ToLower(login), "[bot]") {
		return false
	}
	return !strings.Contains(body, "<!-- workflow-idempotency:")
}

type apiError struct {
	Method     string
	Path       string
	StatusCode int
	Message    string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("github API %s %s: %s", e.Method, e.Path, e.Message)
}

func NewClient(baseURL, token string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Token: token, HTTP: httpClient}
}

// ReadPlan uses GitHub's native sub-issue and blocked-by endpoints. It reads
// every child before validation so an untyped child is observable as an
// incomplete publication rather than silently disappearing from the plan.
func (c *Client) ReadPlan(ctx context.Context, repository string, rootNumber int64) (plan.Snapshot, error) {
	if err := ValidateRepository(repository); err != nil {
		return plan.Snapshot{}, err
	}
	root, err := c.getIssue(ctx, repository, rootNumber)
	if err != nil {
		return plan.Snapshot{}, err
	}
	children, err := c.listIssues(ctx, fmt.Sprintf("/repos/%s/issues/%d/sub_issues", repository, rootNumber))
	if err != nil {
		return plan.Snapshot{}, err
	}
	blockedBy := make(map[int64][]plan.Issue, len(children))
	for _, child := range children {
		blockers, err := c.listIssues(ctx, fmt.Sprintf("/repos/%s/issues/%d/dependencies/blocked_by", repository, child.Number))
		if err != nil {
			return plan.Snapshot{}, err
		}
		blockedBy[child.ID] = blockers
	}
	return plan.Snapshot{Repository: repository, Root: root, Children: children, BlockedBy: blockedBy}, nil
}

func (c *Client) getIssue(ctx context.Context, repository string, number int64) (plan.Issue, error) {
	var raw issueResponse
	if err := c.getJSON(ctx, "/repos/"+repository+"/issues/"+strconv.FormatInt(number, 10), &raw); err != nil {
		return plan.Issue{}, err
	}
	return raw.issue(), nil
}

func (c *Client) listIssues(ctx context.Context, path string) ([]plan.Issue, error) {
	var all []plan.Issue
	for page := 1; ; page++ {
		var raw []issueResponse
		separator := "?"
		if strings.Contains(path, "?") {
			separator = "&"
		}
		if err := c.getJSON(ctx, path+separator+"per_page=100&page="+strconv.Itoa(page), &raw); err != nil {
			return nil, err
		}
		for _, issue := range raw {
			all = append(all, issue.issue())
		}
		if len(raw) < 100 {
			return all, nil
		}
	}
}

func (c *Client) UpdateIssueBody(ctx context.Context, repository string, number int64, body string) error {
	if err := ValidateRepository(repository); err != nil {
		return err
	}
	payload := struct {
		Body string `json:"body"`
	}{Body: body}
	return c.requestJSON(ctx, http.MethodPatch, "/repos/"+repository+"/issues/"+strconv.FormatInt(number, 10), payload, nil)
}

func (c *Client) UpdatePlanProjection(ctx context.Context, repository string, number int64, projection plan.Projection) error {
	if err := ValidateRepository(repository); err != nil {
		return err
	}
	payload := struct {
		Body string `json:"body"`
	}{Body: planProjectionComment(projection)}
	comments, err := c.listIssueComments(ctx, repository, number)
	if err != nil {
		return err
	}
	status, err := planProjectionStatusComment(comments)
	if err != nil {
		return err
	}
	if status == nil {
		return c.requestJSON(ctx, http.MethodPost, "/repos/"+repository+"/issues/"+strconv.FormatInt(number, 10)+"/comments", payload, nil)
	}
	return c.requestJSON(ctx, http.MethodPatch, "/repos/"+repository+"/issues/comments/"+strconv.FormatInt(status.ID, 10), payload, nil)
}

func (c *Client) HasPlanProjection(ctx context.Context, repository string, number int64, projection plan.Projection) (bool, error) {
	marker := planProjectionMarker(projection)
	statusComments := 0
	matched := false
	for page := 1; ; page++ {
		var comments []commentResponse
		path := "/repos/" + repository + "/issues/" + strconv.FormatInt(number, 10) + "/comments?per_page=100&page=" + strconv.Itoa(page)
		if err := c.getJSON(ctx, path, &comments); err != nil {
			return false, err
		}
		for _, comment := range comments {
			if isLegacyPlanProjectionComment(comment) {
				return false, fmt.Errorf("legacy workflow projection comment found")
			}
			if strings.Contains(comment.Body, planProjectionIdentity) {
				statusComments++
				if statusComments > 1 {
					return false, fmt.Errorf("multiple workflow control-plane comments found")
				}
				if strings.Contains(comment.Body, marker) {
					matched = true
				}
			}
		}
		if len(comments) < 100 {
			return matched, nil
		}
	}
}

func planProjectionComment(projection plan.Projection) string {
	content, _ := plan.RenderProjection("", projection)
	return content + "\n\n" + planProjectionIdentity + "\n" + planProjectionMarker(projection)
}

const planProjectionIdentity = "<!-- workflow:control-plane -->"

const planProjectionMarkerPrefix = "workflow-projection:"

func planProjectionStatusComment(comments []commentResponse) (*commentResponse, error) {
	var status *commentResponse
	for index := range comments {
		if isLegacyPlanProjectionComment(comments[index]) {
			return nil, fmt.Errorf("legacy workflow projection comment found")
		}
		if strings.Contains(comments[index].Body, planProjectionIdentity) {
			if status != nil {
				return nil, fmt.Errorf("multiple workflow control-plane comments found")
			}
			status = &comments[index]
		}
	}
	return status, nil
}

func isLegacyPlanProjectionComment(comment commentResponse) bool {
	return strings.Contains(comment.Body, planProjectionMarkerPrefix) && !strings.Contains(comment.Body, planProjectionIdentity)
}

func planProjectionMarker(projection plan.Projection) string {
	content, _ := plan.RenderProjection("", projection)
	digest := sha256.Sum256([]byte(content))
	return fmt.Sprintf("<!-- workflow-projection:%x -->", digest)
}

func (c *Client) listIssueComments(ctx context.Context, repository string, number int64) ([]commentResponse, error) {
	var all []commentResponse
	for page := 1; ; page++ {
		var comments []commentResponse
		path := "/repos/" + repository + "/issues/" + strconv.FormatInt(number, 10) + "/comments?per_page=100&page=" + strconv.Itoa(page)
		if err := c.getJSON(ctx, path, &comments); err != nil {
			return nil, err
		}
		all = append(all, comments...)
		if len(comments) < 100 {
			return all, nil
		}
	}
}

func (c *Client) AddIssueLabel(ctx context.Context, repository string, number int64, label string) error {
	if err := ValidateRepository(repository); err != nil {
		return err
	}
	payload := struct {
		Labels []string `json:"labels"`
	}{Labels: []string{label}}
	return c.requestJSON(ctx, http.MethodPost, "/repos/"+repository+"/issues/"+strconv.FormatInt(number, 10)+"/labels", payload, nil)
}

func (c *Client) getJSON(ctx context.Context, path string, destination any) error {
	return c.requestJSON(ctx, http.MethodGet, path, nil, destination)
}

func (c *Client) requestJSON(ctx context.Context, method, path string, body any, destination any) error {
	return c.requestJSONWithHeaders(ctx, method, path, body, destination, nil)
}

func (c *Client) requestJSONWithHeaders(ctx context.Context, method, path string, body any, destination any, requestHeaders http.Header, responseHeaders ...*http.Header) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = strings.NewReader(string(encoded))
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	for name, values := range requestHeaders {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	response, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if len(responseHeaders) > 0 && responseHeaders[0] != nil {
		*responseHeaders[0] = response.Header.Clone()
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 16<<10))
		return &apiError{Method: method, Path: path, StatusCode: response.StatusCode, Message: strings.TrimSpace(string(message))}
	}
	if destination == nil {
		return nil
	}
	return json.NewDecoder(response.Body).Decode(destination)
}

type issueResponse struct {
	ID        int64           `json:"id"`
	NodeID    string          `json:"node_id"`
	Number    int64           `json:"number"`
	Title     string          `json:"title"`
	Body      string          `json:"body"`
	State     string          `json:"state"`
	UpdatedAt string          `json:"updated_at"`
	Labels    []labelResponse `json:"labels"`
}

type labelResponse struct {
	Name string `json:"name"`
}

func (i issueResponse) issue() plan.Issue {
	labels := make([]string, 0, len(i.Labels))
	for _, label := range i.Labels {
		labels = append(labels, label.Name)
	}
	return plan.Issue{ID: i.ID, NodeID: i.NodeID, Number: i.Number, Title: i.Title, Body: i.Body, State: i.State, Labels: labels, UpdatedAt: i.UpdatedAt}
}

// ValidateRepository prevents accidental path traversal when a repository is
// supplied by configuration. GitHub repository names are owner/name pairs.
func ValidateRepository(repository string) error {
	parts := strings.Split(repository, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("invalid GitHub repository %q", repository)
	}
	for _, part := range parts {
		if _, err := url.PathUnescape(part); err != nil || strings.ContainsAny(part, "/\\?#") {
			return fmt.Errorf("invalid GitHub repository %q", repository)
		}
	}
	return nil
}
