package onboarding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/skyhuang233/workflow/internal/setupcontract"
	"github.com/skyhuang233/workflow/internal/store"
)

// RepositoryRemote is the narrow external boundary used by Repository
// Onboarding. It intentionally cannot express a platform installation,
// credential persistence, Docker, PATH, or Control Plane mutation.
type RepositoryRemote interface {
	Repository(context.Context, string) (RepositoryPolicy, error)
	DefaultBranchHead(context.Context, string) (RepositoryBranch, error)
	OnboardingPull(context.Context, string, string, string, []RequiredCheck) (PullReadback, error)
	OnboardingBranch(context.Context, string, string) (RepositoryBranch, bool, error)
	CreateOrUpdateOnboardingPull(context.Context, OnboardingPullRequest) (PullReadback, error)
	MergeOnboardingPull(context.Context, string, int64, string, string) (string, error)
	VerifyOnboardingContent(context.Context, string, string, map[string][]byte) error
	CreateRepository(context.Context, string, string, string, bool) error
	ReconcileLabel(context.Context, string, Label) error
	ReconcileFeatures(context.Context, string, bool, bool, string) error
	VerifyContract(context.Context, string, string, string) error
	Label(context.Context, string, string) (Label, error)
	Features(context.Context, string) (bool, bool, string, error)
	Variable(context.Context, string, string) (string, error)
	ReconcileVariable(context.Context, string, string, string) error
}

// ErrRepositoryNotFound is the only remote-read outcome that authorizes the
// immutable create_repository effect. Transports must map only typed 404s.
var (
	ErrRepositoryNotFound         = errors.New("repository not found")
	ErrManagedLabelNotFound       = errors.New("managed label not found")
	ErrRepositoryVariableNotFound = errors.New("repository variable not found")
	ErrManagedContentNotFound     = errors.New("managed repository content not found")
	ErrRepositoryContractNotFound = errors.New("repository contract not found")
)

// RepositoryBranch is a readback of a GitHub branch ref and its object ID.
// The adapter deliberately carries both values: a default branch name alone is
// not authority to apply or merge an immutable Onboarding Plan.
type RepositoryBranch struct {
	Name string
	Head string
}

// OnboardingPullRequest contains only the immutable identity required to
// create or re-read an Onboarding Pull Request. It intentionally has no merge
// method: approved merge authority is a later, separately guarded transition.
type OnboardingPullRequest struct {
	Repository, Branch, Head, Base, BaseHead, Digest string
	Files                                            map[string][]byte
	RequiredChecks                                   []RequiredCheck
}

// PreparedOnboardingBranch is a deterministic local commit. Cleanup is kept
// with the prepared clone so a PAT never needs to be persisted in the adapter.
type PreparedOnboardingBranch struct {
	Branch  string
	Head    string
	Cleanup func() error
	publish func(context.Context, string, string, GitCredential) error
}

// OnboardingBranchWriter is the local Git seam. Its production implementation
// uses the hardened, credential-scoped primitives in gitworkspace.go; fakes
// can assert the exact branch/ref payload without launching Git.
type OnboardingBranchWriter interface {
	Prepare(context.Context, OnboardingPullRequest, string, GitCredential) (PreparedOnboardingBranch, error)
	Publish(context.Context, PreparedOnboardingBranch, string, string, GitCredential) error
}

type RepositoryAdapter struct {
	Remote         RepositoryRemote
	Credential     GitCredential
	Owner          string
	PlanDigest     string
	Store          *store.Store
	MergeHeads     map[string]string
	Created        map[string]bool
	BaselineHead   map[string]string
	RepositoryPath string
	BranchWriter   OnboardingBranchWriter
}

func (a *RepositoryAdapter) Readback(ctx context.Context, effect setupcontract.Effect) (setupcontract.EffectStatus, string, error) {
	if a.Remote == nil || !isFullSHA256(a.PlanDigest) {
		return setupcontract.EffectFailed, "", errors.New("Repository Onboarding remote and exact digest are required")
	}
	switch effect.Kind {
	case "create_repository":
		policy, err := a.Remote.Repository(ctx, effect.Subject)
		if err != nil {
			if errors.Is(err, ErrRepositoryNotFound) {
				return setupcontract.EffectRequired, "repository is absent", nil
			}
			return setupcontract.EffectFailed, "", err
		}
		// Repository creation is recovered from the owner-guarded remote identity
		// and approved visibility, never from a process-local "Created" bit.
		if policy.Private != (effect.Parameters["private"] == "true") {
			return setupcontract.EffectConflicting, "repository differs from approved creation", nil
		}
		return setupcontract.EffectSatisfied, "repository creation identity is verified", nil
	case "github_label":
		actual, err := a.Remote.Label(ctx, strings.Split(effect.Subject, "#")[0], effect.Parameters["name"])
		if err != nil {
			if errors.Is(err, ErrManagedLabelNotFound) {
				return setupcontract.EffectRequired, "managed label is absent", nil
			}
			return setupcontract.EffectFailed, "", err
		}
		if !strings.EqualFold(actual.Color, effect.Parameters["color"]) || actual.Description != effect.Parameters["description"] {
			return setupcontract.EffectRequired, "managed label differs", nil
		}
		return setupcontract.EffectSatisfied, "managed label matches", nil
	case "repository_features":
		issues, actions, allowed, err := a.Remote.Features(ctx, effect.Subject)
		if err != nil {
			if errors.Is(err, ErrRepositoryNotFound) {
				return setupcontract.EffectRequired, "repository features are absent with the repository", nil
			}
			return setupcontract.EffectFailed, "", err
		}
		if !issues || !actions || allowed != effect.Parameters["allowed_actions"] {
			return setupcontract.EffectRequired, "repository features differ", nil
		}
		return setupcontract.EffectSatisfied, "repository features match", nil
	case "repository_variable":
		actual, err := a.Remote.Variable(ctx, effect.Subject, effect.Parameters["name"])
		if errors.Is(err, ErrRepositoryVariableNotFound) {
			return setupcontract.EffectRequired, "repository variable is absent", nil
		}
		if err != nil {
			return setupcontract.EffectFailed, "", err
		}
		if actual != effect.Parameters["value"] {
			return setupcontract.EffectRequired, "repository variable differs", nil
		}
		return setupcontract.EffectSatisfied, "repository variable matches", nil
	case "repository_contract_pr":
		return a.readbackOnboardingPull(ctx, effect)
	case "repository_admission":
		if a.Store == nil {
			return setupcontract.EffectFailed, "", errors.New("generation-local store is required for Repository Admission")
		}
		record, err := a.Store.RepositoryAdmission(ctx, effect.Subject)
		if err != nil {
			return setupcontract.EffectRequired, "Repository Admission record is absent", nil
		}
		if record.OnboardingPlanDigestSHA256 != a.PlanDigest || record.ContractVersion != effect.Parameters["contract_version"] || record.ManifestDigestSHA256 != effect.Parameters["manifest_digest"] || !record.Eligible {
			return setupcontract.EffectConflicting, "Repository Admission record differs from the exact approved onboarding", nil
		}
		if _, err := a.Store.RepositoryRuntimeConfiguration(ctx, effect.Subject); errors.Is(err, store.ErrNotFound) {
			return setupcontract.EffectRequired, "Repository Runtime Configuration seed is absent", nil
		} else if err != nil {
			return setupcontract.EffectFailed, "", err
		}
		if err := a.Remote.VerifyContract(ctx, effect.Subject, effect.Parameters["default_branch"], effect.Parameters["manifest_digest"]); err != nil {
			if errors.Is(err, ErrRepositoryContractNotFound) {
				return setupcontract.EffectRequired, "managed Repository Contract is absent", nil
			}
			return setupcontract.EffectFailed, "", err
		}
		return setupcontract.EffectSatisfied, "Repository Contract is live", nil
	case "initial_baseline":
		heads, err := gitOutput(ctx, effect.Subject, "rev-list", "--max-parents=0", "HEAD")
		if err != nil {
			return setupcontract.EffectRequired, "Initial Repository Baseline is absent", nil
		}
		roots := strings.Fields(heads)
		if len(roots) != 1 || !isGitObjectID(roots[0]) {
			return setupcontract.EffectConflicting, "Initial Repository Baseline history is ambiguous", nil
		}
		head := roots[0]
		var files []BaselineFile
		if json.Unmarshal([]byte(effect.Parameters["files_json"]), &files) != nil {
			return setupcontract.EffectFailed, "", errors.New("invalid Initial Repository Baseline files")
		}
		if err := VerifyInitialBaseline(ctx, effect.Subject, head, files); err != nil {
			return setupcontract.EffectConflicting, "Initial Repository Baseline differs from approved files", nil
		}
		if a.BaselineHead == nil {
			a.BaselineHead = map[string]string{}
		}
		a.BaselineHead[effect.ID] = head
		return setupcontract.EffectSatisfied, "Initial Repository Baseline matches approved files", nil
	case "publish_history":
		head, err := gitOutput(ctx, a.RepositoryPath, "rev-parse", "--verify", "HEAD")
		if err != nil {
			return setupcontract.EffectFailed, "", err
		}
		if head != effect.Parameters["head"] {
			return setupcontract.EffectConflicting, "local committed history differs from approved head", nil
		}
		return setupcontract.EffectRequired, "approved history requires publication", nil
	case "local_fast_forward":
		merge := a.MergeHeads[effect.Parameters["merge_head_effect_id"]]
		if merge == "" {
			return setupcontract.EffectFailed, "", errors.New("approved Onboarding Pull Request merge head is absent")
		}
		remote, remoteErr := a.Remote.DefaultBranchHead(ctx, effect.Parameters["repository"])
		if remoteErr != nil {
			return setupcontract.EffectFailed, "", remoteErr
		}
		if remote.Name != effect.Parameters["branch"] || remote.Head != merge {
			return setupcontract.EffectConflicting, "GitHub default branch differs from the approved onboarding merge", nil
		}
		head, err := gitOutput(ctx, effect.Subject, "rev-parse", "--verify", "HEAD")
		if err != nil {
			return setupcontract.EffectFailed, "", err
		}
		if head == merge {
			return setupcontract.EffectSatisfied, "local default branch matches approved merged head", nil
		}
		return setupcontract.EffectRequired, "local default branch requires safe fast-forward", nil
	default:
		return setupcontract.EffectFailed, "", fmt.Errorf("unsupported Repository Onboarding effect %q", effect.Kind)
	}
}

func (a *RepositoryAdapter) readbackOnboardingPull(ctx context.Context, effect setupcontract.Effect) (setupcontract.EffectStatus, string, error) {
	baseHead, err := a.approvedOnboardingBase(effect)
	if err != nil {
		return setupcontract.EffectFailed, "", err
	}
	base := effect.Parameters["base_branch"]
	branch := "workflow/onboarding-" + a.PlanDigest[:12]
	defaultBranch, err := a.Remote.DefaultBranchHead(ctx, effect.Subject)
	if err != nil {
		return setupcontract.EffectFailed, "", err
	}
	if defaultBranch.Name != base || !isGitObjectID(defaultBranch.Head) {
		return setupcontract.EffectConflicting, "default branch identity is incomplete or differs from the approved base", nil
	}
	checks, checkErr := requiredChecksForEffect(effect)
	if checkErr != nil {
		return setupcontract.EffectFailed, "", checkErr
	}
	pull, err := a.Remote.OnboardingPull(ctx, effect.Subject, branch, base, checks)
	if err != nil {
		return setupcontract.EffectFailed, "", err
	}
	if pull.Found {
		files, decodeErr := decodeManagedFiles(effect.Parameters["files_json"])
		if decodeErr != nil {
			return setupcontract.EffectFailed, "", decodeErr
		}
		contentRef := branch
		if pull.Merged && isGitObjectID(pull.Head) {
			contentRef = pull.Head
		}
		if verifyErr := a.Remote.VerifyOnboardingContent(ctx, effect.Subject, contentRef, files); verifyErr != nil {
			if errors.Is(verifyErr, ErrManagedContentNotFound) {
				pull.ContentMatches = false
			} else {
				return setupcontract.EffectFailed, "", verifyErr
			}
		}
	}
	decision, err := DecideOnboardingPull(pull, a.PlanDigest, branch, base, baseHead)
	if err != nil || decision == PullConflict {
		return setupcontract.EffectConflicting, "Onboarding Pull Request identity or content drifted from the exact approved digest", nil
	}
	switch decision {
	case PullMissing:
		if defaultBranch.Head != baseHead {
			return setupcontract.EffectConflicting, "default branch advanced from the approved Onboarding Pull Request base", nil
		}
		return setupcontract.EffectRequired, "Onboarding Pull Request is absent for the exact approved digest", nil
	case PullDrift:
		if defaultBranch.Head != baseHead {
			return setupcontract.EffectConflicting, "default branch advanced from the approved Onboarding Pull Request base", nil
		}
		return setupcontract.EffectRequired, "exact Onboarding Pull Request is not yet an admitted merge", nil
	case PullSatisfied:
		if defaultBranch.Name != base || defaultBranch.Head != pull.MergeHead {
			return setupcontract.EffectConflicting, "default branch differs from the exact Onboarding Pull Request merge", nil
		}
		if err := a.Remote.VerifyContract(ctx, effect.Subject, base, effect.Parameters["manifest_digest"]); err != nil {
			return setupcontract.EffectConflicting, "managed Repository Contract differs after onboarding merge", nil
		}
		if a.MergeHeads == nil {
			a.MergeHeads = map[string]string{}
		}
		if persisted := a.MergeHeads[effect.ID]; persisted != "" && persisted != pull.MergeHead {
			return setupcontract.EffectConflicting, "Onboarding Pull Request merge head conflicts with persisted evidence", nil
		}
		a.MergeHeads[effect.ID] = pull.MergeHead
		return setupcontract.EffectSatisfied, "exact Onboarding Pull Request merge and managed content are verified", nil
	default:
		return setupcontract.EffectFailed, "", errors.New("unknown Onboarding Pull Request decision")
	}
}

func requiredChecksForEffect(effect setupcontract.Effect) ([]RequiredCheck, error) {
	var checks []RequiredCheck
	if err := json.Unmarshal([]byte(effect.Parameters["required_checks_json"]), &checks); err != nil || len(checks) == 0 {
		return nil, errors.New("Onboarding Pull Request lacks approved required checks")
	}
	return CanonicalRequiredChecks(checks), nil
}

func (a *RepositoryAdapter) approvedOnboardingBase(effect setupcontract.Effect) (string, error) {
	baseHead := effect.Parameters["base_head"]
	if baseHead == "" {
		baselineID := effect.Parameters["base_head_effect_id"]
		baseHead = a.BaselineHead[baselineID]
		if baselineID == "" || baseHead == "" {
			return "", errors.New("persisted Initial Repository Baseline head is required as the Onboarding Pull Request base")
		}
	}
	if !isGitObjectID(baseHead) {
		return "", errors.New("approved Onboarding Pull Request base head is invalid")
	}
	return baseHead, nil
}

func (a *RepositoryAdapter) Apply(ctx context.Context, effect setupcontract.Effect) error {
	if a.Remote == nil || !isFullSHA256(a.PlanDigest) {
		return errors.New("Repository Onboarding remote and exact digest are required")
	}
	switch effect.Kind {
	case "create_repository":
		if err := a.Remote.CreateRepository(ctx, effect.Parameters["owner"], effect.Parameters["authenticated_login"], effect.Parameters["name"], effect.Parameters["private"] == "true"); err != nil {
			return err
		}
		if a.Created == nil {
			a.Created = map[string]bool{}
		}
		a.Created[effect.Subject] = true
	case "github_label":
		return a.Remote.ReconcileLabel(ctx, strings.Split(effect.Subject, "#")[0], Label{Name: effect.Parameters["name"], Color: effect.Parameters["color"], Description: effect.Parameters["description"]})
	case "repository_features":
		return a.Remote.ReconcileFeatures(ctx, effect.Subject, effect.Parameters["issues"] == "true", effect.Parameters["actions"] == "true", effect.Parameters["allowed_actions"])
	case "repository_variable":
		return a.Remote.ReconcileVariable(ctx, effect.Subject, effect.Parameters["name"], effect.Parameters["value"])
	case "repository_contract_pr":
		return a.applyOnboardingPull(ctx, effect)
	case "repository_admission":
		if a.Store == nil {
			return errors.New("generation-local store is required for Repository Admission")
		}
		// Admission eligibility is the durable result of a live remote contract
		// verification, never a promise to verify later. A failed readback leaves
		// no eligible record behind for a retry to misinterpret as authority.
		if err := a.Remote.VerifyContract(ctx, effect.Subject, effect.Parameters["default_branch"], effect.Parameters["manifest_digest"]); err != nil {
			return err
		}
		now := nowUTC()
		return a.Store.RecordRepositoryAdmissionWithInitialRuntimeConfiguration(ctx, store.RepositoryAdmission{Repository: effect.Subject, OnboardingPlanDigestSHA256: a.PlanDigest, ContractVersion: effect.Parameters["contract_version"], ManifestDigestSHA256: effect.Parameters["manifest_digest"], Eligible: true, VerifiedAt: now}, store.RepositoryRuntimeConfiguration{
			Repository:         effect.Subject,
			DefaultBranch:      effect.Parameters["default_branch"],
			SourcePath:         a.RepositoryPath,
			GitHubAPIURL:       "https://api.github.com",
			PollInterval:       time.Minute,
			WorkspaceRetention: 7 * 24 * time.Hour,
			MaxParallelRuns:    1,
			UpdatedAt:          now,
		})
	case "initial_baseline":
		var files []BaselineFile
		if err := json.Unmarshal([]byte(effect.Parameters["files_json"]), &files); err != nil {
			return err
		}
		if _, err := gitOutput(ctx, effect.Subject, "rev-parse", "--verify", "HEAD"); err != nil {
			head, createErr := CreateInitialBaseline(ctx, effect.Subject, effect.Parameters["branch"], files, "Initial Repository Baseline\n\nOnboarding-Plan-SHA256: "+a.PlanDigest)
			if createErr != nil {
				return createErr
			}
			if a.BaselineHead == nil {
				a.BaselineHead = map[string]string{}
			}
			a.BaselineHead[effect.ID] = head
		}
		return PublishDefaultBranch(ctx, effect.Subject, effect.Parameters["source_url"], effect.Parameters["branch"], a.Credential)
	case "publish_history":
		return PublishDefaultBranch(ctx, a.RepositoryPath, "https://github.com/"+effect.Subject+".git", effect.Parameters["branch"], a.Credential)
	case "local_fast_forward":
		merge := a.MergeHeads[effect.Parameters["merge_head_effect_id"]]
		if merge == "" {
			return errors.New("approved merge head is required")
		}
		remote, err := a.Remote.DefaultBranchHead(ctx, effect.Parameters["repository"])
		if err != nil {
			return err
		}
		if remote.Name != effect.Parameters["branch"] || remote.Head != merge {
			return errors.New("GitHub default branch differs from the approved onboarding merge")
		}
		preMergeHead := effect.Parameters["pre_merge_head"]
		if preMergeHead == "" {
			baselineEffectID := effect.Parameters["pre_merge_head_effect_id"]
			if baselineEffectID == "" {
				// Compatibility for an already-approved zero-commit v0.0.1 Plan.
				baselineEffectID = "initial-baseline"
			}
			preMergeHead = a.BaselineHead[baselineEffectID]
		}
		if !isGitObjectID(preMergeHead) {
			return errors.New("approved local pre-merge head evidence is unavailable")
		}
		return SafeFastForward(ctx, effect.Subject, effect.Parameters["repository"], effect.Parameters["branch"], preMergeHead, merge, a.Credential)
	default:
		return fmt.Errorf("Repository Onboarding effect %q requires its guarded Git primitive", effect.Kind)
	}
	return nil
}

func nowUTC() time.Time { return time.Now().UTC() }
