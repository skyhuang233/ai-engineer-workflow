package github

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/skyhuang233/workflow/internal/onboarding"
)

type OnboardingRepository struct {
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	Private       bool   `json:"private"`
	HasIssues     bool   `json:"has_issues"`
	Archived      bool   `json:"archived"`
	Disabled      bool   `json:"disabled"`
	Permissions   struct {
		Admin    bool `json:"admin"`
		Maintain bool `json:"maintain"`
		Push     bool `json:"push"`
	} `json:"permissions"`
	AllowSquashMerge bool `json:"allow_squash_merge"`
	AllowMergeCommit bool `json:"allow_merge_commit"`
	AllowRebaseMerge bool `json:"allow_rebase_merge"`
}
type ManagedLabel struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}
type PullRequestCreate struct {
	Title string `json:"title"`
	Head  string `json:"head"`
	Base  string `json:"base"`
	Body  string `json:"body"`
}
type OnboardingPullRequest struct {
	Number         int64  `json:"number"`
	State          string `json:"state"`
	Body           string `json:"body"`
	MergedAt       string `json:"merged_at"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	Mergeable      *bool  `json:"mergeable"`
	MergeableState string `json:"mergeable_state"`
	Head           struct {
		SHA string `json:"sha"`
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		SHA string `json:"sha"`
		Ref string `json:"ref"`
	} `json:"base"`
}
type PullRequestReview struct {
	State string `json:"state"`
}
type MergeResult struct {
	Merged  bool   `json:"merged"`
	SHA     string `json:"sha"`
	Message string `json:"message"`
}

type ActionsPermissions struct {
	Enabled        bool   `json:"enabled"`
	AllowedActions string `json:"allowed_actions"`
}
type SelectedActionsPermissions struct {
	GitHubOwnedAllowed bool `json:"github_owned_allowed"`
}
type BranchProtection struct {
	RequiredStatusChecks *struct {
		Contexts []string `json:"contexts"`
		Checks   []struct {
			Context string `json:"context"`
		} `json:"checks"`
	} `json:"required_status_checks"`
	RequiredPullRequestReviews *struct {
		RequiredApprovingReviewCount int `json:"required_approving_review_count"`
	} `json:"required_pull_request_reviews"`
}
type RepositoryRuleset struct {
	Enforcement string `json:"enforcement"`
	Rules       []struct {
		Type       string `json:"type"`
		Parameters struct {
			RequiredApprovingReviewCount int `json:"required_approving_review_count"`
			RequiredStatusChecks         []struct {
				Context string `json:"context"`
			} `json:"required_status_checks"`
		} `json:"parameters"`
	} `json:"rules"`
}

func (c *Client) DiscoverPolicy(ctx context.Context, repository, branch string) (onboarding.RepositoryPolicy, error) {
	result := onboarding.RepositoryPolicy{}
	repo, err := c.RepositoryForOnboarding(ctx, repository)
	if err != nil {
		return result, err
	}
	if repo.Archived || repo.Disabled {
		return result, errors.New("repository is archived or disabled")
	}
	result.HasIssues, result.Admin = repo.HasIssues, repo.Permissions.Admin
	result.AllowSquashMerge, result.AllowMergeCommit, result.AllowRebaseMerge = repo.AllowSquashMerge, repo.AllowMergeCommit, repo.AllowRebaseMerge
	if branch == "" {
		branch = repo.DefaultBranch
	}
	var actions ActionsPermissions
	if err := c.RequestJSON(ctx, http.MethodGet, "/repos/"+repository+"/actions/permissions", nil, &actions); err != nil {
		return result, err
	}
	switch actions.AllowedActions {
	case "all":
		result.GitHubOwnedActionsAllowed = true
	case "selected":
		var selected SelectedActionsPermissions
		if err := c.RequestJSON(ctx, http.MethodGet, "/repos/"+repository+"/actions/permissions/selected-actions", nil, &selected); err != nil {
			return result, fmt.Errorf("discover selected GitHub Actions policy: %w", err)
		}
		if !selected.GitHubOwnedAllowed {
			return result, errors.New("GitHub Actions policy does not allow the GitHub-owned checkout action")
		}
		result.GitHubOwnedActionsAllowed = true
	case "local_only":
		return result, errors.New("GitHub Actions local_only policy does not allow the GitHub-owned checkout action")
	default:
		return result, errors.New("GitHub Actions allowed_actions policy is unavailable or unsupported")
	}
	result.ActionsEnabled = actions.Enabled
	result.ActionsAllowed = actions.AllowedActions
	var protection BranchProtection
	if err := c.RequestJSON(ctx, http.MethodGet, "/repos/"+repository+"/branches/"+url.PathEscape(branch)+"/protection", nil, &protection); err != nil && !IsNotFound(err) {
		return result, err
	}
	if protection.RequiredPullRequestReviews != nil && protection.RequiredPullRequestReviews.RequiredApprovingReviewCount > 0 {
		result.RequiredHumanReviews = true
	}
	if protection.RequiredStatusChecks != nil {
		result.RequiredChecks = append(result.RequiredChecks, protection.RequiredStatusChecks.Contexts...)
		for _, check := range protection.RequiredStatusChecks.Checks {
			result.RequiredChecks = append(result.RequiredChecks, check.Context)
		}
	}
	var rulesets []RepositoryRuleset
	if err := c.RequestJSON(ctx, http.MethodGet, "/repos/"+repository+"/rulesets?includes_parents=true", nil, &rulesets); err != nil {
		return result, err
	}
	for _, ruleset := range rulesets {
		if ruleset.Enforcement != "active" {
			continue
		}
		for _, rule := range ruleset.Rules {
			switch rule.Type {
			case "merge_queue":
				result.MergeQueue = true
			case "pull_request":
				if rule.Parameters.RequiredApprovingReviewCount > 0 {
					result.RequiredHumanReviews = true
				}
			case "required_status_checks":
				for _, check := range rule.Parameters.RequiredStatusChecks {
					result.RequiredChecks = append(result.RequiredChecks, check.Context)
				}
			}
		}
	}
	result.RequiredChecks = uniquePolicyChecks(result.RequiredChecks)
	return result, nil
}

func uniquePolicyChecks(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}

func IsNotFound(err error) bool {
	var api *apiError
	return errors.As(err, &api) && api.StatusCode == http.StatusNotFound
}
func IsConflict(err error) bool {
	var api *apiError
	return errors.As(err, &api) && api.StatusCode == http.StatusConflict
}
func (c *Client) Label(ctx context.Context, repository, name string) (ManagedLabel, error) {
	var result ManagedLabel
	err := c.RequestJSON(ctx, http.MethodGet, "/repos/"+repository+"/labels/"+url.PathEscape(name), nil, &result)
	return result, err
}
func (c *Client) RepositoryFile(ctx context.Context, repository, path, ref string) ([]byte, error) {
	var result struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	endpoint := "/repos/" + repository + "/contents/" + strings.TrimLeft(path, "/")
	if ref != "" {
		endpoint += "?ref=" + url.QueryEscape(ref)
	}
	if err := c.RequestJSON(ctx, http.MethodGet, endpoint, nil, &result); err != nil {
		return nil, err
	}
	if result.Encoding != "base64" {
		return nil, errors.New("GitHub repository content is not base64")
	}
	return base64.StdEncoding.DecodeString(strings.ReplaceAll(result.Content, "\n", ""))
}

func (c *Client) RepositoryForOnboarding(ctx context.Context, repository string) (OnboardingRepository, error) {
	var result OnboardingRepository
	if err := ValidateRepository(repository); err != nil {
		return result, err
	}
	err := c.RequestJSON(ctx, http.MethodGet, "/repos/"+repository, nil, &result)
	return result, err
}
func (c *Client) CreateRepository(ctx context.Context, owner, authenticatedLogin, name string, private bool) (OnboardingRepository, error) {
	var result OnboardingRepository
	if strings.TrimSpace(name) == "" || strings.Contains(name, "/") {
		return result, errors.New("repository name is invalid")
	}
	path := "/user/repos"
	if !strings.EqualFold(owner, authenticatedLogin) {
		path = "/orgs/" + url.PathEscape(owner) + "/repos"
	}
	body := map[string]any{"name": name, "private": private, "has_issues": true, "auto_init": false}
	if err := c.RequestJSON(ctx, http.MethodPost, path, body, &result); err != nil {
		return result, err
	}
	if !strings.EqualFold(result.FullName, owner+"/"+name) {
		return result, errors.New("created repository identity differs from approved target")
	}
	return result, nil
}

// PreflightCreateRepository proves the approved owner/name is absent and, for
// an organization, that the authenticated member may create the requested
// visibility. It is deliberately read-only and fails closed on omitted policy.
func (c *Client) PreflightCreateRepository(ctx context.Context, owner, authenticatedLogin, name string, private bool) error {
	if strings.TrimSpace(owner) == "" || strings.TrimSpace(authenticatedLogin) == "" || strings.TrimSpace(name) == "" || strings.Contains(name, "/") {
		return errors.New("repository publication preflight identity is incomplete")
	}
	if _, err := c.RepositoryForOnboarding(ctx, owner+"/"+name); err == nil {
		return errors.New("approved GitHub repository name already exists")
	} else if !IsNotFound(err) {
		return fmt.Errorf("preflight approved GitHub repository absence: %w", err)
	}
	if strings.EqualFold(owner, authenticatedLogin) {
		return nil
	}
	var organization struct {
		Login                               string `json:"login"`
		MembersCanCreateRepositories        bool   `json:"members_can_create_repositories"`
		MembersCanCreatePrivateRepositories bool   `json:"members_can_create_private_repositories"`
		MembersCanCreatePublicRepositories  bool   `json:"members_can_create_public_repositories"`
	}
	if err := c.RequestJSON(ctx, http.MethodGet, "/orgs/"+url.PathEscape(owner), nil, &organization); err != nil {
		return fmt.Errorf("discover organization repository-creation policy: %w", err)
	}
	if !strings.EqualFold(organization.Login, owner) {
		return errors.New("organization preflight returned a different owner")
	}
	var membership struct {
		State string `json:"state"`
		Role  string `json:"role"`
	}
	if err := c.RequestJSON(ctx, http.MethodGet, "/user/memberships/orgs/"+url.PathEscape(owner), nil, &membership); err != nil {
		return fmt.Errorf("discover authenticated organization membership: %w", err)
	}
	if membership.State != "active" {
		return errors.New("authenticated organization membership is not active")
	}
	if membership.Role != "admin" && (membership.Role != "member" || !organization.MembersCanCreateRepositories) {
		return errors.New("organization policy forbids member repository creation")
	}
	if membership.Role != "admin" && private && !organization.MembersCanCreatePrivateRepositories {
		return errors.New("organization policy forbids member private repository creation")
	}
	if membership.Role != "admin" && !private && !organization.MembersCanCreatePublicRepositories {
		return errors.New("organization policy forbids member public repository creation")
	}
	var actions struct {
		EnabledRepositories string `json:"enabled_repositories"`
		AllowedActions      string `json:"allowed_actions"`
	}
	if err := c.RequestJSON(ctx, http.MethodGet, "/orgs/"+url.PathEscape(owner)+"/actions/permissions", nil, &actions); err != nil {
		return fmt.Errorf("discover organization Actions policy: %w", err)
	}
	if actions.EnabledRepositories != "all" {
		return errors.New("organization Actions policy does not prove a new repository will be enabled")
	}
	switch actions.AllowedActions {
	case "all":
	case "selected":
		var selected SelectedActionsPermissions
		if err := c.RequestJSON(ctx, http.MethodGet, "/orgs/"+url.PathEscape(owner)+"/actions/permissions/selected-actions", nil, &selected); err != nil {
			return fmt.Errorf("discover organization selected Actions policy: %w", err)
		}
		if !selected.GitHubOwnedAllowed {
			return errors.New("organization Actions policy does not prove required onboarding actions include GitHub-owned checkout")
		}
	case "local_only":
		return errors.New("organization Actions policy does not prove required onboarding actions include GitHub-owned checkout")
	default:
		return errors.New("organization Actions policy does not prove required onboarding actions are allowed")
	}
	var rulesets []RepositoryRuleset
	if err := c.RequestJSON(ctx, http.MethodGet, "/orgs/"+url.PathEscape(owner)+"/rulesets?includes_parents=true", nil, &rulesets); err != nil {
		return fmt.Errorf("discover organization review and merge-queue policy: %w", err)
	}
	for _, ruleset := range rulesets {
		if ruleset.Enforcement != "active" {
			continue
		}
		for _, rule := range ruleset.Rules {
			if rule.Type == "merge_queue" {
				return errors.New("organization ruleset may require an unsupported merge queue")
			}
			if rule.Type == "pull_request" && rule.Parameters.RequiredApprovingReviewCount > 0 {
				return errors.New("organization ruleset may require human review of the Onboarding Pull Request")
			}
		}
	}
	return nil
}
func (c *Client) UpdateRepositoryFeatures(ctx context.Context, repository string, issues, actions bool, allowedActions string) error {
	if err := ValidateRepository(repository); err != nil {
		return err
	}
	if err := c.RequestJSON(ctx, http.MethodPatch, "/repos/"+repository, map[string]any{"has_issues": issues}, nil); err != nil {
		return err
	}
	if actions {
		switch allowedActions {
		case "all", "local_only", "selected":
		default:
			return errors.New("approved Actions allowed_actions policy is required")
		}
		return c.RequestJSON(ctx, http.MethodPut, "/repos/"+repository+"/actions/permissions", map[string]any{"enabled": true, "allowed_actions": allowedActions}, nil)
	}
	return nil
}
func (c *Client) CreateLabel(ctx context.Context, repository string, label ManagedLabel) error {
	if err := ValidateRepository(repository); err != nil {
		return err
	}
	if label.Name == "" || label.Color == "" {
		return errors.New("label name and color are required")
	}
	return c.RequestJSON(ctx, http.MethodPost, "/repos/"+repository+"/labels", label, nil)
}
func (c *Client) UpdateLabel(ctx context.Context, repository, current string, label ManagedLabel) error {
	if err := ValidateRepository(repository); err != nil {
		return err
	}
	return c.RequestJSON(ctx, http.MethodPatch, "/repos/"+repository+"/labels/"+url.PathEscape(current), map[string]string{"new_name": label.Name, "color": label.Color, "description": label.Description}, nil)
}
func (c *Client) CreateOnboardingPullRequest(ctx context.Context, repository string, value PullRequestCreate) (OnboardingPullRequest, error) {
	var result OnboardingPullRequest
	if err := ValidateRepository(repository); err != nil {
		return result, err
	}
	if value.Head == "" || value.Base == "" || value.Title == "" {
		return result, errors.New("pull request identity is incomplete")
	}
	err := c.RequestJSON(ctx, http.MethodPost, "/repos/"+repository+"/pulls", value, &result)
	return result, err
}
func (c *Client) FindOnboardingPullRequest(ctx context.Context, repository, owner, branch, base string) (OnboardingPullRequest, bool, error) {
	var result []OnboardingPullRequest
	if err := ValidateRepository(repository); err != nil {
		return OnboardingPullRequest{}, false, err
	}
	endpoint := "/repos/" + repository + "/pulls?state=all&head=" + url.QueryEscape(owner+":"+branch) + "&base=" + url.QueryEscape(base) + "&per_page=100"
	if err := c.RequestJSON(ctx, http.MethodGet, endpoint, nil, &result); err != nil {
		return OnboardingPullRequest{}, false, err
	}
	if len(result) == 0 {
		return OnboardingPullRequest{}, false, nil
	}
	if len(result) != 1 {
		return OnboardingPullRequest{}, false, errors.New("multiple Onboarding Pull Requests match the approved branch")
	}
	return result[0], true, nil
}
func (c *Client) OnboardingPullRequest(ctx context.Context, repository string, number int64) (OnboardingPullRequest, error) {
	var result OnboardingPullRequest
	if number <= 0 {
		return result, errors.New("pull request number is required")
	}
	err := c.RequestJSON(ctx, http.MethodGet, "/repos/"+repository+"/pulls/"+strconv.FormatInt(number, 10), nil, &result)
	return result, err
}
func (c *Client) OnboardingPullRequestReviews(ctx context.Context, repository string, number int64) ([]PullRequestReview, error) {
	var result []PullRequestReview
	if number <= 0 {
		return nil, errors.New("pull request number is required")
	}
	err := c.RequestJSON(ctx, http.MethodGet, "/repos/"+repository+"/pulls/"+strconv.FormatInt(number, 10)+"/reviews?per_page=100", nil, &result)
	return result, err
}
func (c *Client) MergeOnboardingPullRequest(ctx context.Context, repository string, number int64, expectedHead, method string) (MergeResult, error) {
	var result MergeResult
	if !fullCommitID(expectedHead) {
		return result, errors.New("expected pull request head must be a full commit")
	}
	switch method {
	case "merge", "squash", "rebase":
	default:
		return result, errors.New("unsupported merge method")
	}
	body := map[string]string{"sha": expectedHead, "merge_method": method}
	if err := c.RequestJSON(ctx, http.MethodPut, "/repos/"+repository+"/pulls/"+strconv.FormatInt(number, 10)+"/merge", body, &result); err != nil {
		return result, err
	}
	if !result.Merged {
		return result, fmt.Errorf("GitHub did not merge onboarding pull request: %s", result.Message)
	}
	return result, nil
}
func (c *Client) DeleteBranch(ctx context.Context, repository, branch string) error {
	if err := ValidateRepository(repository); err != nil {
		return err
	}
	return c.RequestJSON(ctx, http.MethodDelete, "/repos/"+repository+"/git/refs/heads/"+url.PathEscape(branch), nil, nil)
}
func fullCommitID(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
