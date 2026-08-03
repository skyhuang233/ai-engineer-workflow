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
	"time"

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
	return c.ActionablePullRequestFeedbackSince(ctx, repository, number, time.Time{}, true)
}

func (c *Client) ActionablePullRequestFeedbackSince(ctx context.Context, repository string, number int64, since time.Time, full bool) ([]PullRequestFeedback, error) {
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
		ID          int64  `json:"id"`
		Body        string `json:"body"`
		State       string `json:"state"`
		User        user   `json:"user"`
		SubmittedAt string `json:"submitted_at"`
	}
	type comment struct {
		ID        int64  `json:"id"`
		Body      string `json:"body"`
		User      user   `json:"user"`
		UpdatedAt string `json:"updated_at"`
	}
	var result []PullRequestFeedback
	for page := 1; ; page++ {
		var reviews []review
		path := "/repos/" + repository + "/pulls/" + strconv.FormatInt(number, 10) + "/reviews?per_page=100&page=" + strconv.Itoa(page)
		if err := c.getJSON(ctx, path, &reviews); err != nil {
			return nil, err
		}
		for _, value := range reviews {
			if value.State != "PENDING" && actionableReview(value.User.Login, value.User.Type, value.Body) && (full || changedSince(value.SubmittedAt, since)) {
				result = append(result, PullRequestFeedback{Source: "review", EventID: strconv.FormatInt(value.ID, 10), Author: value.User.Login, Body: reviewFeedbackBody(value.State, value.Body)})
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
			if !full && !since.IsZero() {
				path += "&since=" + url.QueryEscape(since.UTC().Format(time.RFC3339))
			}
			if err := c.getJSON(ctx, path, &comments); err != nil {
				return nil, err
			}
			for _, value := range comments {
				if actionableComment(value.User.Login, value.User.Type, value.Body) && (full || changedSince(value.UpdatedAt, since)) {
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

func (c *Client) UpdatedPullRequestsSince(ctx context.Context, repository string, since time.Time, full bool) (map[int64]struct{}, error) {
	if err := ValidateRepository(repository); err != nil {
		return nil, err
	}
	type issue struct {
		Number      int64     `json:"number"`
		PullRequest *struct{} `json:"pull_request"`
	}
	updated := make(map[int64]struct{})
	for page := 1; ; page++ {
		path := "/repos/" + repository + "/issues?state=all&per_page=100&page=" + strconv.Itoa(page)
		if !full && !since.IsZero() {
			path += "&since=" + url.QueryEscape(since.UTC().Format(time.RFC3339))
		}
		var issues []issue
		if err := c.getJSON(ctx, path, &issues); err != nil {
			return nil, err
		}
		for _, value := range issues {
			if value.PullRequest != nil {
				updated[value.Number] = struct{}{}
			}
		}
		if len(issues) < 100 {
			return updated, nil
		}
	}
}

func changedSince(value string, since time.Time) bool {
	if since.IsZero() || value == "" {
		return true
	}
	parsed, err := time.Parse(time.RFC3339, value)
	return err != nil || !parsed.Before(since)
}

func actionableReview(login, accountType, body string) bool {
	return actionableAuthor(login, accountType) && !strings.Contains(body, "<!-- workflow-idempotency:")
}

func actionableComment(login, accountType, body string) bool {
	return strings.TrimSpace(body) != "" && actionableReview(login, accountType, body)
}

func actionableAuthor(login, accountType string) bool {
	if strings.EqualFold(accountType, "bot") || strings.HasSuffix(strings.ToLower(login), "[bot]") {
		return false
	}
	return true
}

func reviewFeedbackBody(state, body string) string {
	if strings.TrimSpace(body) != "" {
		return body
	}
	return "Review submitted with state: " + state
}

type apiError struct {
	Method     string
	Path       string
	StatusCode int
	Message    string
	RetryAt    time.Time
}

func (e *apiError) Error() string {
	return fmt.Sprintf("github API %s %s: %s", e.Method, e.Path, e.Message)
}

func NewClient(baseURL, token string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Token: token, HTTP: httpClient}
}

const workflowInboxLabel = "workflow:inbox"

func (c *Client) WorkflowInboxAnswers(ctx context.Context, repository string, questionIDs []string) (map[string]string, error) {
	if err := ValidateRepository(repository); err != nil {
		return nil, err
	}
	if len(questionIDs) == 0 {
		return map[string]string{}, nil
	}
	known := make(map[string]struct{}, len(questionIDs))
	for _, questionID := range questionIDs {
		known[questionID] = struct{}{}
	}
	inbox, found, err := c.workflowInbox(ctx, repository)
	if err != nil {
		return nil, err
	}
	if !found {
		return map[string]string{}, nil
	}
	comments, err := c.listIssueComments(ctx, repository, inbox.Number)
	if err != nil {
		return nil, err
	}
	answers := make(map[string]string)
	for _, comment := range comments {
		if !actionableAuthor(comment.User.Login, comment.User.Type) {
			continue
		}
		for questionID, answer := range parseWorkflowInboxAnswers(comment.Body) {
			if _, ok := known[questionID]; ok {
				answers[questionID] = answer
			}
		}
	}
	return answers, nil
}

func (c *Client) ProjectWorkflowInbox(ctx context.Context, repository string, questions []plan.WorkflowQuestion) error {
	if err := ValidateRepository(repository); err != nil {
		return err
	}
	inbox, found, err := c.workflowInbox(ctx, repository)
	if err != nil {
		return err
	}
	body := plan.RenderWorkflowInbox(questions)
	if found {
		return c.requestJSON(ctx, http.MethodPatch, "/repos/"+repository+"/issues/"+strconv.FormatInt(inbox.Number, 10), map[string]string{"title": "Workflow Inbox", "body": body, "state": "open"}, nil)
	}
	payload := struct {
		Title  string   `json:"title"`
		Body   string   `json:"body"`
		Labels []string `json:"labels"`
	}{Title: "Workflow Inbox", Body: body, Labels: []string{workflowInboxLabel}}
	return c.requestJSON(ctx, http.MethodPost, "/repos/"+repository+"/issues", payload, nil)
}

func (c *Client) HasWorkflowInboxProjection(ctx context.Context, repository string, questions []plan.WorkflowQuestion) (bool, error) {
	inbox, found, err := c.workflowInbox(ctx, repository)
	if err != nil || !found {
		return false, err
	}
	return inbox.Body == plan.RenderWorkflowInbox(questions), nil
}

func (c *Client) workflowInbox(ctx context.Context, repository string) (plan.Issue, bool, error) {
	issues, err := c.listIssues(ctx, "/repos/"+repository+"/issues?state=all&labels="+url.QueryEscape(workflowInboxLabel)+"&per_page=100")
	if err != nil {
		return plan.Issue{}, false, err
	}
	var inboxes []plan.Issue
	for _, issue := range issues {
		for _, label := range issue.Labels {
			if label == workflowInboxLabel {
				inboxes = append(inboxes, issue)
				break
			}
		}
	}
	if len(inboxes) > 1 {
		return plan.Issue{}, false, fmt.Errorf("multiple repository Workflow Inbox issues found")
	}
	if len(inboxes) == 0 {
		return plan.Issue{}, false, nil
	}
	return inboxes[0], true, nil
}

func parseWorkflowInboxAnswers(body string) map[string]string {
	answers := make(map[string]string)
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "workflow-answer:") {
			continue
		}
		parts := strings.SplitN(strings.TrimSpace(strings.TrimPrefix(line, "workflow-answer:")), ":", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
			continue
		}
		answers[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
	}
	return answers
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
	snapshot := plan.Snapshot{Repository: repository, Root: root, Children: children, BlockedBy: blockedBy}
	return c.hydrateDeliveredIssues(ctx, snapshot)
}

func (c *Client) hydrateDeliveredIssues(ctx context.Context, snapshot plan.Snapshot) (plan.Snapshot, error) {
	closed := make(map[int64]struct{})
	for _, issue := range snapshot.Children {
		if strings.EqualFold(issue.State, "closed") {
			closed[issue.Number] = struct{}{}
		}
	}
	if len(closed) == 0 {
		return snapshot, nil
	}
	type pull struct {
		Number   int64  `json:"number"`
		Body     string `json:"body"`
		MergedAt string `json:"merged_at"`
		Base     struct {
			Ref string `json:"ref"`
		} `json:"base"`
	}
	delivered := make(map[int64]bool)
	for page := 1; ; page++ {
		var pulls []pull
		path := "/repos/" + snapshot.Repository + "/pulls?state=closed&base=main&per_page=100&page=" + strconv.Itoa(page)
		if err := c.getJSON(ctx, path, &pulls); err != nil {
			return plan.Snapshot{}, err
		}
		for _, pull := range pulls {
			if pull.MergedAt == "" || pull.Base.Ref != "main" {
				continue
			}
			for number := range closed {
				if closesIssue(pull.Body, number) {
					delivered[number] = true
				}
			}
		}
		if len(pulls) < 100 {
			break
		}
	}
	for index := range snapshot.Children {
		snapshot.Children[index].Delivered = delivered[snapshot.Children[index].Number]
	}
	for blocked, blockers := range snapshot.BlockedBy {
		for index := range blockers {
			blockers[index].Delivered = delivered[blockers[index].Number]
		}
		snapshot.BlockedBy[blocked] = blockers
	}
	return snapshot, nil
}

func closesIssue(body string, number int64) bool {
	lower := strings.ToLower(body)
	for _, verb := range []string{"fixes", "fixed", "closes", "closed", "resolves", "resolved"} {
		if strings.Contains(lower, verb+" #"+strconv.FormatInt(number, 10)) {
			return true
		}
	}
	return false
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
		return &apiError{Method: method, Path: path, StatusCode: response.StatusCode, Message: strings.TrimSpace(string(message)), RetryAt: rateLimitRetryAt(response, time.Now().UTC())}
	}
	if destination == nil {
		return nil
	}
	return json.NewDecoder(response.Body).Decode(destination)
}

func rateLimitRetryAt(response *http.Response, now time.Time) time.Time {
	if response.StatusCode != http.StatusTooManyRequests && response.StatusCode != http.StatusForbidden {
		return time.Time{}
	}
	if retryAfter, err := strconv.Atoi(response.Header.Get("Retry-After")); err == nil && retryAfter > 0 {
		return now.Add(time.Duration(retryAfter) * time.Second)
	}
	if response.Header.Get("X-RateLimit-Remaining") != "0" {
		return time.Time{}
	}
	reset, err := strconv.ParseInt(response.Header.Get("X-RateLimit-Reset"), 10, 64)
	if err != nil || reset <= 0 {
		return time.Time{}
	}
	return time.Unix(reset, 0).UTC()
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
