package github

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
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

var ErrRepositoryOwnerMismatch = errors.New("repository owner does not match configured owner")

type Client struct {
	BaseURL         string
	Token           string
	HTTP            *http.Client
	RepositoryOwner string
}

type RepositoryMetadata struct {
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	Private       bool   `json:"private"`
	Owner         struct {
		Login string `json:"login"`
	} `json:"owner"`
}

type PullRequestFeedback struct {
	Source   string
	EventID  string
	Author   string
	Body     string
	BatchID  string
	Debounce bool
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
		ID                  int64  `json:"id"`
		Body                string `json:"body"`
		User                user   `json:"user"`
		UpdatedAt           string `json:"updated_at"`
		PullRequestReviewID int64  `json:"pull_request_review_id"`
	}
	var result []PullRequestFeedback
	submittedReviewIDs := make(map[int64]struct{})
	for page := 1; ; page++ {
		var reviews []review
		path := "/repos/" + repository + "/pulls/" + strconv.FormatInt(number, 10) + "/reviews?per_page=100&page=" + strconv.Itoa(page)
		if err := c.getJSON(ctx, path, &reviews); err != nil {
			return nil, err
		}
		for _, value := range reviews {
			submitted := value.State != "PENDING" && actionableReview(c.RepositoryOwner, value.User.Login, value.User.Type, value.Body)
			if submitted {
				submittedReviewIDs[value.ID] = struct{}{}
			}
			if submitted && (full || changedSince(value.SubmittedAt, since)) {
				result = append(result, PullRequestFeedback{Source: "review", EventID: strconv.FormatInt(value.ID, 10), Author: value.User.Login, Body: reviewFeedbackBody(value.State, value.Body), BatchID: reviewSubmissionBatchID(value.ID), Debounce: true})
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
				if actionableComment(c.RepositoryOwner, value.User.Login, value.User.Type, value.Body) && (full || changedSince(value.UpdatedAt, since)) {
					event := PullRequestFeedback{Source: endpoint.source, EventID: strconv.FormatInt(value.ID, 10), Author: value.User.Login, Body: value.Body}
					if endpoint.source == "inline-comment" {
						if _, submitted := submittedReviewIDs[value.PullRequestReviewID]; submitted {
							event.BatchID = reviewSubmissionBatchID(value.PullRequestReviewID)
						}
						event.Debounce = true
					}
					result = append(result, event)
				}
			}
			if len(comments) < 100 {
				break
			}
		}
	}
	return result, nil
}

func reviewSubmissionBatchID(reviewID int64) string {
	return "review-submission:" + strconv.FormatInt(reviewID, 10)
}

func changedSince(value string, since time.Time) bool {
	if since.IsZero() || value == "" {
		return true
	}
	parsed, err := time.Parse(time.RFC3339, value)
	return err != nil || !parsed.Before(since)
}

func actionableReview(owner, login, accountType, body string) bool {
	return actionableAuthor(owner, login, accountType) && !strings.Contains(body, "<!-- workflow-idempotency:")
}

func actionableComment(owner, login, accountType, body string) bool {
	return strings.TrimSpace(body) != "" && actionableReview(owner, login, accountType, body)
}

func actionableAuthor(owner, login, accountType string) bool {
	if strings.EqualFold(accountType, "bot") || strings.HasSuffix(strings.ToLower(login), "[bot]") {
		return false
	}
	return strings.TrimSpace(owner) != "" && strings.EqualFold(strings.TrimSpace(login), strings.TrimSpace(owner))
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
	Body       string
	RetryAt    time.Time
}

func (e *apiError) Error() string {
	detail := e.Message
	if detail == "" {
		detail = e.Body
	}
	return fmt.Sprintf("github API %s %s returned %d: %s", e.Method, e.Path, e.StatusCode, detail)
}

func (e *apiError) AuthenticationFailure() bool {
	return e.StatusCode == http.StatusUnauthorized || (e.StatusCode == http.StatusForbidden && e.RetryAt.IsZero())
}

func (e *apiError) RetryAtTime() time.Time {
	return e.RetryAt
}

type APIError = apiError

func NewClient(baseURL, token string, httpClient *http.Client) *Client {
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Token: token, HTTP: httpClient}
}

func (c *Client) WithRepositoryOwner(owner string) *Client {
	configured := *c
	configured.RepositoryOwner = strings.TrimSpace(owner)
	return &configured
}

func (c *Client) RequireOwnerGuardedRepository(ctx context.Context, repository string) error {
	if err := ValidateOwnerGuardedRepositoryName(repository, c.RepositoryOwner); err != nil {
		return err
	}
	var metadata RepositoryMetadata
	if err := c.getJSON(ctx, "/repos/"+repository, &metadata); err != nil {
		return fmt.Errorf("verify Owner-Guarded repository access: %w", err)
	}
	return metadata.ValidateOwnerGuarded(repository, c.RepositoryOwner)
}

func ValidateOwnerGuardedRepositoryName(repository, owner string) error {
	if err := ValidateRepository(repository); err != nil {
		return err
	}
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return fmt.Errorf("configured repository owner is required for GitHub observations")
	}
	repositoryOwner := strings.SplitN(repository, "/", 2)[0]
	if !strings.EqualFold(repositoryOwner, owner) {
		return fmt.Errorf("%w: repository owner %q, configured owner %q", ErrRepositoryOwnerMismatch, repositoryOwner, owner)
	}
	return nil
}

func (m RepositoryMetadata) ValidateOwnerGuarded(repository, owner string) error {
	if err := ValidateOwnerGuardedRepositoryName(repository, owner); err != nil {
		return err
	}
	if !strings.EqualFold(m.FullName, repository) || !strings.EqualFold(m.Owner.Login, strings.TrimSpace(owner)) {
		return fmt.Errorf("%w: canonical repository %q owned by %q, configured repository %q owned by %q", ErrRepositoryOwnerMismatch, m.FullName, m.Owner.Login, repository, owner)
	}
	return nil
}

const workflowInboxLabel = "workflow:inbox"

func (c *Client) WorkflowInboxAnswers(ctx context.Context, repository string, questionIDs []string) (map[string]string, error) {
	if err := c.requireRepositoryOwner(); err != nil {
		return nil, err
	}
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
		if !actionableAuthor(c.RepositoryOwner, comment.User.Login, comment.User.Type) {
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
	if err := c.requireRepositoryOwner(); err != nil {
		return err
	}
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
	if err := c.requireRepositoryOwner(); err != nil {
		return false, err
	}
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
		if !actionableAuthor(c.RepositoryOwner, issue.Author, issue.AuthorType) {
			continue
		}
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

func (c *Client) RequestJSON(ctx context.Context, method, path string, body, destination any) error {
	return c.requestJSON(ctx, method, path, body, destination)
}

func (c *Client) RequestBytes(ctx context.Context, path, accept string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	response, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if readErr != nil {
		return nil, readErr
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		detail := strings.TrimSpace(string(data))
		return nil, &apiError{Method: http.MethodGet, Path: path, StatusCode: response.StatusCode, Message: detail, Body: detail, RetryAt: rateLimitRetryAt(response, detail, time.Now().UTC())}
	}
	return data, nil
}

// ReadPlan uses GitHub's native sub-issue and blocked-by endpoints. It reads
// every child before validation so an untyped child is observable as an
// incomplete publication rather than silently disappearing from the plan.
func (c *Client) ReadPlan(ctx context.Context, repository string, rootNumber int64) (plan.Snapshot, error) {
	if err := ValidateRepository(repository); err != nil {
		return plan.Snapshot{}, err
	}
	if strings.TrimSpace(c.RepositoryOwner) == "" {
		return plan.Snapshot{}, fmt.Errorf("configured repository owner is required to admit a plan")
	}
	root, err := c.getIssue(ctx, repository, rootNumber)
	if err != nil {
		return plan.Snapshot{}, err
	}
	if err := c.requirePlanAuthor(root); err != nil {
		return plan.Snapshot{}, err
	}
	children, err := c.listIssues(ctx, fmt.Sprintf("/repos/%s/issues/%d/sub_issues", repository, rootNumber))
	if err != nil {
		return plan.Snapshot{}, err
	}
	blockedBy := make(map[int64][]plan.Issue, len(children))
	for _, child := range children {
		if err := c.requirePlanAuthor(child); err != nil {
			return plan.Snapshot{}, err
		}
		blockers, err := c.listIssues(ctx, fmt.Sprintf("/repos/%s/issues/%d/dependencies/blocked_by", repository, child.Number))
		if err != nil {
			return plan.Snapshot{}, err
		}
		for _, blocker := range blockers {
			if err := c.requirePlanAuthor(blocker); err != nil {
				return plan.Snapshot{}, err
			}
		}
		blockedBy[child.ID] = blockers
	}
	return plan.Snapshot{Repository: repository, Root: root, Children: children, BlockedBy: blockedBy}, nil
}

func (c *Client) requirePlanAuthor(issue plan.Issue) error {
	if !actionableAuthor(c.RepositoryOwner, issue.Author, issue.AuthorType) {
		return fmt.Errorf("plan issue #%d author %q is not the configured repository owner", issue.Number, issue.Author)
	}
	return nil
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
	if err := c.requireRepositoryOwner(); err != nil {
		return err
	}
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
	status, err := planProjectionStatusComment(comments, c.RepositoryOwner)
	if err != nil {
		return err
	}
	if status == nil {
		return c.requestJSON(ctx, http.MethodPost, "/repos/"+repository+"/issues/"+strconv.FormatInt(number, 10)+"/comments", payload, nil)
	}
	return c.requestJSON(ctx, http.MethodPatch, "/repos/"+repository+"/issues/comments/"+strconv.FormatInt(status.ID, 10), payload, nil)
}

func (c *Client) HasPlanProjection(ctx context.Context, repository string, number int64, projection plan.Projection) (bool, error) {
	if err := c.requireRepositoryOwner(); err != nil {
		return false, err
	}
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
			if !actionableAuthor(c.RepositoryOwner, comment.User.Login, comment.User.Type) {
				continue
			}
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

func (c *Client) requireRepositoryOwner() error {
	if strings.TrimSpace(c.RepositoryOwner) == "" {
		return fmt.Errorf("configured repository owner is required for GitHub observations")
	}
	return nil
}

func planProjectionComment(projection plan.Projection) string {
	content, _ := plan.RenderProjection("", projection)
	return content + "\n\n" + planProjectionIdentity + "\n" + planProjectionMarker(projection)
}

const planProjectionIdentity = "<!-- workflow:control-plane -->"

const planProjectionMarkerPrefix = "workflow-projection:"

func planProjectionStatusComment(comments []commentResponse, owner string) (*commentResponse, error) {
	var status *commentResponse
	for index := range comments {
		if !actionableAuthor(owner, comments[index].User.Login, comments[index].User.Type) {
			continue
		}
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
		detail := strings.TrimSpace(string(message))
		return &apiError{Method: method, Path: path, StatusCode: response.StatusCode, Message: detail, Body: detail, RetryAt: rateLimitRetryAt(response, detail, time.Now().UTC())}
	}
	if destination == nil {
		return nil
	}
	return json.NewDecoder(response.Body).Decode(destination)
}

func rateLimitRetryAt(response *http.Response, detail string, now time.Time) time.Time {
	if response.StatusCode != http.StatusTooManyRequests && response.StatusCode != http.StatusForbidden {
		return time.Time{}
	}
	if retryAfter, err := strconv.Atoi(response.Header.Get("Retry-After")); err == nil && retryAfter > 0 {
		return now.Add(time.Duration(retryAfter) * time.Second)
	}
	if response.Header.Get("X-RateLimit-Remaining") == "0" {
		reset, err := strconv.ParseInt(response.Header.Get("X-RateLimit-Reset"), 10, 64)
		if err == nil && reset > 0 {
			return time.Unix(reset, 0).UTC()
		}
		return now.Add(time.Minute)
	}
	if response.StatusCode == http.StatusTooManyRequests || isSecondaryRateLimitMessage(detail) {
		return now.Add(time.Minute)
	}
	return time.Time{}
}

func isSecondaryRateLimitMessage(detail string) bool {
	message := strings.ToLower(detail)
	return strings.Contains(message, "secondary rate limit") || strings.Contains(message, "abuse detection mechanism")
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
	User      userResponse    `json:"user"`
}

type userResponse struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

type labelResponse struct {
	Name string `json:"name"`
}

func (i issueResponse) issue() plan.Issue {
	labels := make([]string, 0, len(i.Labels))
	for _, label := range i.Labels {
		labels = append(labels, label.Name)
	}
	return plan.Issue{ID: i.ID, NodeID: i.NodeID, Number: i.Number, Title: i.Title, Body: i.Body, State: i.State, Labels: labels, UpdatedAt: i.UpdatedAt, Author: i.User.Login, AuthorType: i.User.Type}
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
