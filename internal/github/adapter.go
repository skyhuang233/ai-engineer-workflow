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
	var status *commentResponse
	for index := range comments {
		if strings.Contains(comments[index].Body, planProjectionIdentity) {
			if status != nil {
				return fmt.Errorf("multiple workflow control-plane comments found")
			}
			status = &comments[index]
		}
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
			if strings.Contains(comment.Body, planProjectionIdentity) {
				statusComments++
				if statusComments > 1 {
					return false, fmt.Errorf("multiple workflow control-plane comments found")
				}
				if strings.Contains(comment.Body, marker) {
					matched = true
				}
				continue
			}
			if strings.Contains(comment.Body, marker) {
				return false, fmt.Errorf("legacy workflow projection comment found")
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
