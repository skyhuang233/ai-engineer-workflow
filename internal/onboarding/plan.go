package onboarding

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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
	Publication           PublicationPreflight
	State                 StateDiscovery
	DomainLayout          string
}
type Label struct{ Name, Color, Description string }
type RequiredCheck struct {
	Context string `json:"context"`
	AppID   int64  `json:"app_id"`
}

// GitHubActionsAppID is GitHub's stable public integration identity for
// check-runs produced by Actions workflows, including workflow-contract.
const GitHubActionsAppID int64 = 15368

type RepositoryPolicy struct {
	HasIssues, ActionsEnabled, Admin                     bool
	AllowSquashMerge, AllowMergeCommit, AllowRebaseMerge bool
	RequiredHumanReviews, MergeQueue                     bool
	ActionsAllowed                                       string
	GitHubOwnedActionsAllowed                            bool
	RequiredChecks                                       []RequiredCheck
	AllowFeatureEnable                                   bool
}
type PolicyDiscovery interface {
	DiscoverPolicy(context.Context, string, string) (RepositoryPolicy, error)
}

type PublicationPreflight interface {
	PreflightCreateRepository(context.Context, string, string, string, bool) error
}

type OnboardingState struct {
	SatisfiedLabels    map[string]bool
	ContractSatisfied  bool
	AdmissionSatisfied bool
}

type StateDiscovery interface {
	DiscoverOnboardingState(context.Context, string, string, string, []Label) (OnboardingState, error)
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
	} else if data, readErr := os.ReadFile(filepath.Join(discovery.Root, "AGENTS.md")); readErr == nil {
		baseAgents = data
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
		} else if existing, readErr := os.ReadFile(filepath.Join(discovery.Root, filepath.FromSlash(path))); readErr == nil {
			beforePayload[path] = base64.StdEncoding.EncodeToString(existing)
		}
	}
	encodedFiles, _ := json.Marshal(filePayload)
	encodedBeforeFiles, _ := json.Marshal(beforePayload)
	var zeroBaseline []BaselineFile
	if !discovery.HasCommits {
		zeroBaseline, err = BaselineSnapshot(ctx, discovery.Root)
		if err != nil {
			return setupcontract.Plan{}, err
		}
	}
	snapshotJSON, err := CaptureApprovalSnapshot(ctx, discovery, repositoryID, zeroBaseline)
	if err != nil {
		return setupcontract.Plan{}, err
	}
	plan := setupcontract.Plan{SchemaVersion: 1, Kind: setupcontract.RepositoryOnboarding, Target: setupcontract.Target{WorkflowHome: options.WorkflowHome, RepositoryPath: discovery.Root, GitHubRepository: repositoryID}, Preconditions: []setupcontract.Precondition{{ID: "platform-release", Kind: "platform_release", Subject: options.WorkflowHome, Expected: options.PlatformReleaseDigest}, {ID: "onboarding-discovery", Kind: "onboarding_snapshot", Subject: discovery.Root, Expected: snapshotJSON}, {ID: "local-head", Kind: "git_head", Subject: discovery.Root, Expected: discovery.Head}}, ExpectedResults: []setupcontract.ExpectedResult{{ID: "repository-admitted", Kind: "repository_admission", Subject: repositoryID, Expected: manifestDigest}}}
	policy := RepositoryPolicy{}
	state := OnboardingState{}
	if discovery.Published && options.Policy == nil {
		return setupcontract.Plan{}, errors.New("published repository requires complete read-only GitHub policy preflight")
	}
	if discovery.Published {
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
		if !policy.Admin {
			return setupcontract.Plan{}, errors.New("published repository onboarding requires verified repository administration")
		}
		if !policy.GitHubOwnedActionsAllowed {
			return setupcontract.Plan{}, errors.New("published repository Actions policy does not prove GitHub-owned checkout")
		}
		if !policy.HasIssues || !policy.ActionsEnabled {
			if !policy.Admin {
				return setupcontract.Plan{}, errors.New("repository administration is required to enable GitHub Issues and Actions")
			}
			if !policy.ActionsEnabled && policy.ActionsAllowed == "" {
				return setupcontract.Plan{}, errors.New("repository Actions allowed_actions policy is unavailable")
			}
			plan.Effects = append(plan.Effects, setupcontract.Effect{ID: "enable-repository-features", Kind: "repository_features", Subject: repositoryID, Action: "enable", Parameters: map[string]string{"issues": "true", "actions": "true", "allowed_actions": policy.ActionsAllowed}})
			policy.AllowFeatureEnable = true
		}
		for _, required := range policy.RequiredChecks {
			if strings.TrimSpace(required.Context) == "" || required.AppID <= 0 {
				return setupcontract.Plan{}, errors.New("repository required check lacks an App identity")
			}
		}
		policy.RequiredChecks = uniqueRequiredChecks(policy.RequiredChecks)
		checkApps := map[string]int64{}
		for _, required := range policy.RequiredChecks {
			if existing := checkApps[required.Context]; existing != 0 && existing != required.AppID {
				return setupcontract.Plan{}, errors.New("repository required check context has conflicting App identities")
			}
			checkApps[required.Context] = required.AppID
		}
		policyJSON, _ := json.Marshal(policy)
		plan.Preconditions = append(plan.Preconditions, setupcontract.Precondition{ID: "github-policy", Kind: "github_policy", Subject: repositoryID, Expected: string(policyJSON)})
	}
	if discovery.Published {
		remoteJSON, _ := json.Marshal(map[string]string{"branch": discovery.DefaultBranch, "head": discovery.RemoteHead, "manifest_digest": manifestDigest})
		plan.Preconditions = append(plan.Preconditions, setupcontract.Precondition{ID: "github-default-head", Kind: "github_default_head", Subject: repositoryID, Expected: string(remoteJSON)})
		if options.State != nil {
			state, err = options.State.DiscoverOnboardingState(ctx, repositoryID, discovery.DefaultBranch, manifestDigest, options.Labels)
			if err != nil {
				return setupcontract.Plan{}, fmt.Errorf("discover current onboarding state: %w", err)
			}
		}
	}
	if !discovery.Published {
		if options.Publication == nil {
			return setupcontract.Plan{}, errors.New("unpublished repository requires a read-only absence preflight")
		}
		if err := options.Publication.PreflightCreateRepository(ctx, options.Owner, options.AuthenticatedLogin, repositoryName, defaultPrivate(options)); err != nil {
			return setupcontract.Plan{}, fmt.Errorf("preflight GitHub repository publication: %w", err)
		}
		plan.Effects = append(plan.Effects, setupcontract.Effect{ID: "create-repository", Kind: "create_repository", Subject: repositoryID, Action: "create", Parameters: map[string]string{"owner": options.Owner, "authenticated_login": options.AuthenticatedLogin, "name": repositoryName, "private": boolString(defaultPrivate(options)), "approval_absent_repository": repositoryID}})
		if !discovery.HasCommits {
			baselineFiles, filesErr := BaselineFiles(ctx, discovery.Root)
			if filesErr != nil {
				return setupcontract.Plan{}, filesErr
			}
			if findings := ScanCredentialMaterial(discovery.Root, baselineFiles); len(findings) > 0 {
				return setupcontract.Plan{}, errors.New("credential material blocks Initial Repository Baseline")
			}
			encoded, _ := json.Marshal(zeroBaseline)
			plan.Effects = append(plan.Effects, setupcontract.Effect{ID: "initial-baseline", Kind: "initial_baseline", Subject: discovery.Root, Action: "commit_and_push", Parameters: map[string]string{"branch": discovery.DefaultBranch, "files_json": string(encoded), "repository": repositoryID, "source_url": "https://github.com/" + repositoryID + ".git"}})
		} else {
			plan.Effects = append(plan.Effects, setupcontract.Effect{ID: "publish-history", Kind: "publish_history", Subject: repositoryID, Action: "push", Parameters: map[string]string{"branch": discovery.DefaultBranch, "head": discovery.Head, "new_repository": "true"}})
		}
	}
	for index, label := range options.Labels {
		if state.SatisfiedLabels[label.Name] {
			continue
		}
		plan.Effects = append(plan.Effects, setupcontract.Effect{ID: "label-" + integer(index), Kind: "github_label", Subject: repositoryID + "#" + label.Name, Action: "reconcile", Parameters: map[string]string{"name": label.Name, "color": label.Color, "description": label.Description}})
	}
	labelsJSON, _ := json.Marshal(options.Labels)
	sourceURL, err := GitHubHTTPSURL(repositoryID)
	if err != nil {
		return setupcontract.Plan{}, err
	}
	requiredChecks := uniqueRequiredChecks(append([]RequiredCheck{{Context: "workflow-contract", AppID: GitHubActionsAppID}}, policy.RequiredChecks...))
	requiredChecksJSON, _ := json.Marshal(requiredChecks)
	if !state.ContractSatisfied {
		parameters := map[string]string{"base_branch": discovery.DefaultBranch, "base_head": discovery.Head, "source_url": sourceURL, "before_files_json": string(encodedBeforeFiles), "files_json": string(encodedFiles), "manifest_digest": manifestDigest, "required_checks_json": string(requiredChecksJSON)}
		if !discovery.HasCommits {
			parameters["base_head_effect_id"] = "initial-baseline"
		}
		plan.Effects = append(plan.Effects, setupcontract.Effect{ID: "repository-contract-pr", Kind: "repository_contract_pr", Subject: repositoryID, Action: "create_check_merge", Parameters: parameters})
	}
	// Every approved onboarding plan terminates in a live admission readback.
	// This also binds label-, feature-, and policy-only repairs to the complete
	// Repository Contract instead of treating their individual delta as Ready.
	plan.Effects = append(plan.Effects, setupcontract.Effect{ID: "record-repository-admission", Kind: "repository_admission", Subject: repositoryID, Action: "verify_and_record", Parameters: map[string]string{"default_branch": discovery.DefaultBranch, "manifest_digest": manifestDigest, "contract_version": "1", "labels_json": string(labelsJSON), "actions_allowed": policy.ActionsAllowed}})
	if !state.ContractSatisfied {
		plan.Effects = append(plan.Effects, setupcontract.Effect{ID: "synchronize-local-default-branch", Kind: "local_fast_forward", Subject: discovery.Root, Action: "fast_forward_if_safe", Parameters: map[string]string{"repository": repositoryID, "branch": discovery.DefaultBranch, "pre_merge_head": discovery.Head, "merge_head_effect_id": "repository-contract-pr"}})
	}
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
func uniqueRequiredChecks(values []RequiredCheck) []RequiredCheck {
	seen := map[string]bool{}
	result := make([]RequiredCheck, 0, len(values))
	for _, value := range values {
		value.Context = strings.TrimSpace(value.Context)
		if value.Context == "" || value.AppID <= 0 || seen[value.Context+":"+fmt.Sprint(value.AppID)] {
			continue
		}
		seen[value.Context+":"+fmt.Sprint(value.AppID)] = true
		result = append(result, value)
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
