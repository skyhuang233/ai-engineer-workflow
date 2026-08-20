package main

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	workflowgithub "github.com/skyhuang233/workflow/internal/github"
	"github.com/skyhuang233/workflow/internal/onboarding"
	"github.com/skyhuang233/workflow/internal/repositorycontract"
	"github.com/skyhuang233/workflow/internal/store"
)

// githubOnboardingRemote translates the narrow onboarding boundary to GitHub's
// owner-guarded client. It has no platform or credential-persistence powers.
type githubOnboardingRemote struct {
	client *workflowgithub.Client
	owner  string
}

func requiredWorkflowLabels() []onboarding.Label {
	return []onboarding.Label{
		{Name: "workflow:inbox", Color: "5319e7", Description: "Agent Workflow inbox"},
		{Name: "workflow:plan", Color: "0e8a16", Description: "Agent Workflow delivery plan"},
		{Name: "workflow:ticket", Color: "1d76db", Description: "Agent Workflow executable ticket"},
		{Name: "workflow:active", Color: "fbca04", Description: "Agent Workflow active work"},
		{Name: "workflow:delivered", Color: "006b75", Description: "Agent Workflow delivered work"},
	}
}

type onboardingCurrentState struct {
	Client *workflowgithub.Client
	Store  *store.Store
}

func (d onboardingCurrentState) DiscoverOnboardingState(ctx context.Context, repository, branch, manifestDigest string, labels []onboarding.Label) (onboarding.OnboardingState, error) {
	if d.Client == nil || d.Store == nil {
		return onboarding.OnboardingState{}, errors.New("onboarding state discovery is incomplete")
	}
	result := onboarding.OnboardingState{SatisfiedLabels: map[string]bool{}}
	for _, expected := range labels {
		actual, err := d.Client.Label(ctx, repository, expected.Name)
		if workflowgithub.IsNotFound(err) {
			continue
		}
		if err != nil {
			return result, err
		}
		result.SatisfiedLabels[expected.Name] = strings.EqualFold(actual.Color, expected.Color) && actual.Description == expected.Description
	}
	var fetchErr error
	_, contractErr := repositorycontract.VerifyRemote(func(path string) ([]byte, error) {
		content, err := d.Client.RepositoryFile(ctx, repository, path, branch)
		if err != nil && fetchErr == nil {
			fetchErr = err
		}
		return content, err
	}, repository, branch, manifestDigest)
	if fetchErr != nil && !workflowgithub.IsNotFound(fetchErr) {
		return result, fetchErr
	}
	result.ContractSatisfied = contractErr == nil
	if admission, err := d.Store.RepositoryAdmission(ctx, repository); err == nil {
		result.AdmissionSatisfied = result.ContractSatisfied && admission.Eligible && admission.ManifestDigestSHA256 == manifestDigest && admission.ContractVersion == "1"
	} else if !errors.Is(err, store.ErrNotFound) {
		return result, err
	}
	return result, nil
}

func (r githubOnboardingRemote) Repository(ctx context.Context, repository string) (onboarding.RepositoryPolicy, error) {
	value, err := r.client.DiscoverPolicy(ctx, repository, "")
	if workflowgithub.IsNotFound(err) {
		return onboarding.RepositoryPolicy{}, onboarding.ErrRepositoryNotFound
	}
	return value, err
}
func (r githubOnboardingRemote) DefaultBranchHead(ctx context.Context, repository string) (onboarding.RepositoryBranch, error) {
	v, err := r.client.DefaultBranchHead(ctx, repository)
	return onboarding.RepositoryBranch{Name: v.Name, Head: v.Head}, err
}
func (r githubOnboardingRemote) OnboardingBranch(ctx context.Context, repository, branch string) (onboarding.RepositoryBranch, bool, error) {
	var value struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	err := r.client.RequestJSON(ctx, http.MethodGet, "/repos/"+repository+"/git/ref/heads/"+url.PathEscape(branch), nil, &value)
	if workflowgithub.IsNotFound(err) {
		return onboarding.RepositoryBranch{}, false, nil
	}
	if err != nil {
		return onboarding.RepositoryBranch{}, false, err
	}
	return onboarding.RepositoryBranch{Name: branch, Head: value.Object.SHA}, true, nil
}
func (r githubOnboardingRemote) OnboardingPull(ctx context.Context, repository, branch, base string, required []onboarding.RequiredCheck) (onboarding.PullReadback, error) {
	pull, found, err := r.client.FindOnboardingPullRequest(ctx, repository, r.owner, branch, base)
	if err != nil || !found {
		return onboarding.PullReadback{Found: found}, err
	}
	value := onboarding.PullReadback{Found: true, Number: pull.Number, Branch: pull.Head.Ref, Head: pull.Head.SHA, Base: pull.Base.Ref, BaseHead: pull.Base.SHA, Body: pull.Body, MergeHead: pull.MergeCommitSHA, State: pull.State, Merged: pull.MergedAt != "", Mergeable: pull.Mergeable != nil && *pull.Mergeable, ContentMatches: true}
	reviews, err := r.client.OnboardingPullRequestReviews(ctx, repository, pull.Number)
	if err != nil {
		return onboarding.PullReadback{}, err
	}
	value.ReviewsClean = true
	for _, review := range reviews {
		if strings.EqualFold(review.State, "CHANGES_REQUESTED") {
			value.ReviewsClean = false
		}
	}
	if value.Head != "" {
		checks, checkErr := r.client.OnboardingChecks(ctx, repository, value.Head)
		if checkErr != nil {
			return onboarding.PullReadback{}, checkErr
		}
		value.ChecksPassed = checksPassed(checks, required)
	}
	return value, nil
}
func checksPassed(checks []workflowgithub.OnboardingCheck, required []onboarding.RequiredCheck) bool {
	if len(required) == 0 {
		return false
	}
	seen := map[string]bool{}
	for _, check := range checks {
		if strings.EqualFold(check.Status, "completed") && strings.EqualFold(check.Conclusion, "success") {
			seen[check.Name+":"+strconv.FormatInt(check.AppID, 10)] = true
		}
	}
	for _, check := range required {
		if !seen[check.Context+":"+strconv.FormatInt(check.AppID, 10)] {
			return false
		}
	}
	return true
}
func (r githubOnboardingRemote) CreateOrUpdateOnboardingPull(ctx context.Context, request onboarding.OnboardingPullRequest) (onboarding.PullReadback, error) {
	_, err := r.client.CreateOnboardingPullRequest(ctx, request.Repository, workflowgithub.PullRequestCreate{Title: "Onboard Agent Workflow", Head: request.Branch, Base: request.Base, Body: "Approved Setup Plan SHA-256: " + request.Digest})
	if err != nil {
		// A concurrent retry may have created the same digest-bound PR between
		// our readback and POST. Re-read it; the adapter validates all identity
		// fields before treating it as idempotent.
		if existing, found, readErr := r.client.FindOnboardingPullRequest(ctx, request.Repository, r.owner, request.Branch, request.Base); readErr == nil && found && existing.Head.SHA == request.Head && existing.Body == "Approved Setup Plan SHA-256: "+request.Digest {
			return r.OnboardingPull(ctx, request.Repository, request.Branch, request.Base, request.RequiredChecks)
		}
		return onboarding.PullReadback{}, err
	}
	return r.OnboardingPull(ctx, request.Repository, request.Branch, request.Base, request.RequiredChecks)
}
func (r githubOnboardingRemote) MergeOnboardingPull(ctx context.Context, repository string, number int64, head, method string) (string, error) {
	value, err := r.client.MergeOnboardingPullRequest(ctx, repository, number, head, method)
	return value.SHA, err
}
func (r githubOnboardingRemote) VerifyOnboardingContent(ctx context.Context, repository, branch string, files map[string][]byte) error {
	for path, expected := range files {
		actual, err := r.client.RepositoryFile(ctx, repository, path, branch)
		if workflowgithub.IsNotFound(err) {
			return onboarding.ErrManagedContentNotFound
		}
		if err != nil || string(actual) != string(expected) {
			if err != nil {
				return err
			}
			return errors.New("Onboarding Pull Request managed content differs")
		}
	}
	return nil
}
func (r githubOnboardingRemote) CreateRepository(ctx context.Context, owner, login, name string, private bool) error {
	_, err := r.client.CreateRepository(ctx, owner, login, name, private)
	return err
}
func (r githubOnboardingRemote) ReconcileLabel(ctx context.Context, repository string, label onboarding.Label) error {
	current, err := r.client.Label(ctx, repository, label.Name)
	if workflowgithub.IsNotFound(err) {
		return r.client.CreateLabel(ctx, repository, workflowgithub.ManagedLabel{Name: label.Name, Color: label.Color, Description: label.Description})
	}
	if err != nil {
		return err
	}
	return r.client.UpdateLabel(ctx, repository, current.Name, workflowgithub.ManagedLabel{Name: label.Name, Color: label.Color, Description: label.Description})
}
func (r githubOnboardingRemote) ReconcileFeatures(ctx context.Context, repository string, issues, actions bool, allowed string) error {
	return r.client.UpdateRepositoryFeatures(ctx, repository, issues, actions, allowed)
}
func (r githubOnboardingRemote) VerifyContract(ctx context.Context, repository, branch, digest string) error {
	missing := false
	_, err := repositorycontract.VerifyRemote(func(path string) ([]byte, error) {
		value, readErr := r.client.RepositoryFile(ctx, repository, path, branch)
		if workflowgithub.IsNotFound(readErr) {
			missing = true
			return nil, onboarding.ErrRepositoryContractNotFound
		}
		return value, readErr
	}, repository, branch, digest)
	if missing {
		return onboarding.ErrRepositoryContractNotFound
	}
	return err
}
func (r githubOnboardingRemote) Label(ctx context.Context, repository, name string) (onboarding.Label, error) {
	value, err := r.client.Label(ctx, repository, name)
	if workflowgithub.IsNotFound(err) {
		return onboarding.Label{}, onboarding.ErrManagedLabelNotFound
	}
	return onboarding.Label{Name: value.Name, Color: value.Color, Description: value.Description}, err
}
func (r githubOnboardingRemote) Features(ctx context.Context, repository string) (bool, bool, string, error) {
	policy, err := r.client.DiscoverPolicy(ctx, repository, "")
	if workflowgithub.IsNotFound(err) {
		return false, false, "", onboarding.ErrRepositoryNotFound
	}
	return policy.HasIssues, policy.ActionsEnabled, policy.ActionsAllowed, err
}
func (r githubOnboardingRemote) Variable(ctx context.Context, repository, name string) (string, error) {
	var value struct {
		Value string `json:"value"`
	}
	err := r.client.RequestJSON(ctx, http.MethodGet, "/repos/"+repository+"/actions/variables/"+url.PathEscape(name), nil, &value)
	if workflowgithub.IsNotFound(err) {
		return "", onboarding.ErrRepositoryVariableNotFound
	}
	return value.Value, err
}
func (r githubOnboardingRemote) ReconcileVariable(ctx context.Context, repository, name, value string) error {
	body := map[string]string{"name": name, "value": value}
	err := r.client.RequestJSON(ctx, http.MethodPatch, "/repos/"+repository+"/actions/variables/"+url.PathEscape(name), body, nil)
	if workflowgithub.IsNotFound(err) {
		return r.client.RequestJSON(ctx, http.MethodPost, "/repos/"+repository+"/actions/variables", body, nil)
	}
	return err
}

type githubRemoteHead struct{ remote githubOnboardingRemote }

func (r githubRemoteHead) Resolve(ctx context.Context, origin string) (string, string, error) {
	repository, err := onboarding.ParseGitHubOrigin(origin)
	if err != nil {
		return "", "", err
	}
	value, err := r.remote.DefaultBranchHead(ctx, repository)
	return value.Name, value.Head, err
}
