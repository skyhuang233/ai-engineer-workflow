package onboarding

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"

	"github.com/skyhuang233/workflow/internal/repositorycontract"
	"github.com/skyhuang233/workflow/internal/setupcontract"
)

type PlanOptions struct {
	RepositoryPath, WorkflowHome, Owner, AuthenticatedLogin, RepositoryName string
	// Private is nil when the caller accepts the platform default (private).
	// A non-nil false value is an explicit public-repository choice.
	Private               *bool
	Remote                RemoteHead
	PlatformReleaseDigest string
	Labels                []Label
	Policy                PolicyDiscovery
	DomainLayout          string
}
type Label struct{ Name, Color, Description string }
type RepositoryPolicy struct {
	HasIssues, ActionsEnabled, Admin                     bool
	AllowSquashMerge, AllowMergeCommit, AllowRebaseMerge bool
	RequiredHumanReviews, MergeQueue                     bool
	RequiredChecks                                       []string
	AllowFeatureEnable                                   bool
}
type PolicyDiscovery interface {
	DiscoverPolicy(context.Context, string, string) (RepositoryPolicy, error)
}

func Plan(ctx context.Context, options PlanOptions) (setupcontract.Plan, error) {
	if options.RepositoryPath == "" || options.WorkflowHome == "" || options.Owner == "" || len(options.PlatformReleaseDigest) != 64 {
		return setupcontract.Plan{}, errors.New("Onboarding Plan inputs are incomplete")
	}
	discovery, err := Discover(ctx, options.RepositoryPath, options.Remote)
	if err != nil {
		return setupcontract.Plan{}, err
	}
	repositoryID := discovery.Repository
	repositoryName := options.RepositoryName
	if repositoryName == "" {
		repositoryName = filepath.Base(discovery.Root)
	}
	if repositoryID == "" {
		repositoryID = options.Owner + "/" + repositoryName
	} else if !strings.EqualFold(strings.SplitN(repositoryID, "/", 2)[0], options.Owner) {
		return setupcontract.Plan{}, errors.New("repository owner differs from the Workflow Home owner binding")
	}
	baseAgents := []byte(nil)
	if discovery.HasCommits {
		if data, showErr := gitBytes(ctx, discovery.Root, "show", "HEAD:AGENTS.md"); showErr == nil {
			baseAgents = data
		}
	}
	files, _, manifestDigest, err := repositorycontract.Render(options.DomainLayout, baseAgents, repositoryID, discovery.DefaultBranch)
	if err != nil {
		return setupcontract.Plan{}, err
	}
	filePayload := map[string]string{}
	beforePayload := map[string]string{}
	for path, data := range files {
		filePayload[path] = base64.StdEncoding.EncodeToString(data)
		if discovery.HasCommits {
			if existing, showErr := gitBytes(ctx, discovery.Root, "show", "HEAD:"+path); showErr == nil {
				beforePayload[path] = base64.StdEncoding.EncodeToString(existing)
			}
		}
	}
	encodedFiles, _ := json.Marshal(filePayload)
	encodedBeforeFiles, _ := json.Marshal(beforePayload)
	plan := setupcontract.Plan{SchemaVersion: 1, Kind: setupcontract.RepositoryOnboarding, Target: setupcontract.Target{WorkflowHome: options.WorkflowHome, RepositoryPath: discovery.Root, GitHubRepository: repositoryID}, Preconditions: []setupcontract.Precondition{{ID: "platform-release", Kind: "platform_release", Subject: options.WorkflowHome, Expected: options.PlatformReleaseDigest}, {ID: "local-head", Kind: "git_head", Subject: discovery.Root, Expected: discovery.Head}}, ExpectedResults: []setupcontract.ExpectedResult{{ID: "repository-admitted", Kind: "repository_admission", Subject: repositoryID, Expected: manifestDigest}}}
	policy := RepositoryPolicy{}
	if discovery.Published && options.Policy != nil {
		policy, err = options.Policy.DiscoverPolicy(ctx, repositoryID, discovery.DefaultBranch)
		if err != nil {
			return setupcontract.Plan{}, err
		}
		if policy.RequiredHumanReviews {
			return setupcontract.Plan{}, errors.New("repository policy requires a human review of the Onboarding Pull Request")
		}
		if policy.MergeQueue {
			return setupcontract.Plan{}, errors.New("repository policy requires an unsupported merge queue")
		}
		if !policy.AllowSquashMerge && !policy.AllowMergeCommit && !policy.AllowRebaseMerge {
			return setupcontract.Plan{}, errors.New("repository has no supported merge method")
		}
		if !policy.HasIssues || !policy.ActionsEnabled {
			if !policy.Admin {
				return setupcontract.Plan{}, errors.New("repository administration is required to enable GitHub Issues and Actions")
			}
			plan.Effects = append(plan.Effects, setupcontract.Effect{ID: "enable-repository-features", Kind: "repository_features", Subject: repositoryID, Action: "enable", Parameters: map[string]string{"issues": "true", "actions": "true"}})
			policy.AllowFeatureEnable = true
		}
		policy.RequiredChecks = uniqueStrings(policy.RequiredChecks)
		policyJSON, _ := json.Marshal(policy)
		plan.Preconditions = append(plan.Preconditions, setupcontract.Precondition{ID: "github-policy", Kind: "github_policy", Subject: repositoryID, Expected: string(policyJSON)})
	}
	if !discovery.Published {
		plan.Effects = append(plan.Effects, setupcontract.Effect{ID: "create-repository", Kind: "create_repository", Subject: repositoryID, Action: "create", Parameters: map[string]string{"owner": options.Owner, "authenticated_login": options.AuthenticatedLogin, "name": repositoryName, "private": boolString(defaultPrivate(options))}})
		if !discovery.HasCommits {
			baselineFiles, filesErr := BaselineFiles(ctx, discovery.Root)
			if filesErr != nil {
				return setupcontract.Plan{}, filesErr
			}
			if findings := ScanCredentialMaterial(discovery.Root, baselineFiles); len(findings) > 0 {
				return setupcontract.Plan{}, errors.New("credential material blocks Initial Repository Baseline")
			}
			encoded, _ := json.Marshal(baselineFiles)
			plan.Effects = append(plan.Effects, setupcontract.Effect{ID: "initial-baseline", Kind: "initial_baseline", Subject: discovery.Root, Action: "commit_and_push", Parameters: map[string]string{"branch": discovery.DefaultBranch, "files_json": string(encoded), "repository": repositoryID, "source_url": "https://github.com/" + repositoryID + ".git"}})
		} else {
			plan.Effects = append(plan.Effects, setupcontract.Effect{ID: "publish-history", Kind: "publish_history", Subject: repositoryID, Action: "push", Parameters: map[string]string{"branch": discovery.DefaultBranch, "head": discovery.Head}})
		}
	}
	for index, label := range options.Labels {
		plan.Effects = append(plan.Effects, setupcontract.Effect{ID: "label-" + integer(index), Kind: "github_label", Subject: repositoryID + "#" + label.Name, Action: "reconcile", Parameters: map[string]string{"name": label.Name, "color": label.Color, "description": label.Description}})
	}
	labelsJSON, _ := json.Marshal(options.Labels)
	sourceURL := discovery.Origin
	if sourceURL == "" {
		sourceURL = "https://github.com/" + repositoryID + ".git"
	}
	requiredChecks := uniqueStrings(append([]string{"workflow-contract"}, policy.RequiredChecks...))
	requiredChecksJSON, _ := json.Marshal(requiredChecks)
	plan.Effects = append(plan.Effects, setupcontract.Effect{ID: "repository-contract-pr", Kind: "repository_contract_pr", Subject: repositoryID, Action: "create_check_merge", Parameters: map[string]string{"base_branch": discovery.DefaultBranch, "base_head": discovery.Head, "source_url": sourceURL, "before_files_json": string(encodedBeforeFiles), "files_json": string(encodedFiles), "manifest_digest": manifestDigest, "required_checks_json": string(requiredChecksJSON)}})
	plan.Effects = append(plan.Effects, setupcontract.Effect{ID: "record-repository-admission", Kind: "repository_admission", Subject: repositoryID, Action: "verify_and_record", Parameters: map[string]string{"default_branch": discovery.DefaultBranch, "manifest_digest": manifestDigest, "contract_version": "1", "labels_json": string(labelsJSON)}})
	plan.Effects = append(plan.Effects, setupcontract.Effect{ID: "synchronize-local-default-branch", Kind: "local_fast_forward", Subject: discovery.Root, Action: "fast_forward_if_safe", Parameters: map[string]string{"repository": repositoryID, "branch": discovery.DefaultBranch}})
	identityJSON, _ := json.Marshal(plan)
	identity := sha256.Sum256(identityJSON)
	plan.PlanID = "onboard-" + hex.EncodeToString(identity[:12])
	return plan, nil
}
func uniqueStrings(values []string) []string {
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
func defaultPrivate(options PlanOptions) bool {
	if options.Private != nil {
		return *options.Private
	}
	return true
}
func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
func integer(value int) string {
	const digits = "0123456789"
	if value < 10 {
		return string(digits[value])
	}
	result := ""
	for value > 0 {
		result = string(digits[value%10]) + result
		value /= 10
	}
	return result
}
