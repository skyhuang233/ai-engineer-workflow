package onboarding

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/skyhuang233/workflow/internal/setupcontract"
)

func (a *RepositoryAdapter) applyOnboardingPull(ctx context.Context, effect setupcontract.Effect) error {
	if err := a.guardOnboardingMutation(effect.Subject); err != nil {
		return err
	}
	baseHead, err := a.approvedOnboardingBase(effect)
	if err != nil {
		return err
	}
	base := effect.Parameters["base_branch"]
	sourceURL, err := a.canonicalOnboardingSource(effect.Subject, effect.Parameters["source_url"])
	if err != nil {
		return err
	}
	files, err := decodeManagedFiles(effect.Parameters["files_json"])
	if err != nil {
		return err
	}
	var checks []RequiredCheck
	if err := json.Unmarshal([]byte(effect.Parameters["required_checks_json"]), &checks); err != nil || checks == nil {
		return errors.New("Onboarding Pull Request approved required checks are invalid")
	}
	request := OnboardingPullRequest{Repository: effect.Subject, Branch: "workflow/onboarding-" + a.PlanDigest[:12], Base: base, BaseHead: baseHead, Digest: a.PlanDigest, Files: files, RequiredChecks: CanonicalRequiredChecks(checks)}
	// A prior exact merge is a read-only retry: bind and verify it before
	// comparing the now-advanced default branch to the pre-merge base.
	prior, err := a.Remote.OnboardingPull(ctx, effect.Subject, request.Branch, base, request.RequiredChecks)
	if err != nil {
		return err
	}
	if prior.Found && a.Remote.VerifyOnboardingContent(ctx, effect.Subject, request.Branch, request.Files) != nil {
		prior.ContentMatches = false
	}
	priorDecision, priorErr := DecideOnboardingPull(prior, a.PlanDigest, request.Branch, base, baseHead)
	if priorErr != nil || priorDecision == PullConflict {
		return errors.New("existing Onboarding Pull Request differs from the approved identity or managed content")
	}
	if priorDecision == PullSatisfied {
		return a.bindMergedOnboardingPull(ctx, effect, request, prior)
	}
	defaultBranch, err := a.Remote.DefaultBranchHead(ctx, effect.Subject)
	if err != nil {
		return err
	}
	if defaultBranch.Name != base || defaultBranch.Head != baseHead {
		return errors.New("GitHub default branch advanced from the approved Onboarding Pull Request base")
	}
	writer := a.BranchWriter
	if writer == nil {
		writer = gitBranchWriter{temporaryRoot: os.TempDir()}
	}
	prepared, err := writer.Prepare(ctx, request, sourceURL, a.Credential)
	if err != nil {
		return err
	}
	if prepared.Cleanup != nil {
		defer prepared.Cleanup()
	}
	if prepared.Branch != request.Branch || !isGitObjectID(prepared.Head) {
		return errors.New("prepared Onboarding branch differs from deterministic approved identity")
	}
	request.Head = prepared.Head

	pull, err := a.Remote.OnboardingPull(ctx, effect.Subject, request.Branch, base, request.RequiredChecks)
	if err != nil {
		return err
	}
	if pull.Found && a.Remote.VerifyOnboardingContent(ctx, effect.Subject, request.Branch, request.Files) != nil {
		pull.ContentMatches = false
	}
	decision, decisionErr := DecideOnboardingPull(pull, a.PlanDigest, request.Branch, base, baseHead)
	if decisionErr != nil || decision == PullConflict {
		return errors.New("existing Onboarding Pull Request differs from the approved identity or managed content")
	}
	if decision == PullSatisfied {
		return a.bindMergedOnboardingPull(ctx, effect, request, pull)
	}
	if pull.Found && pull.Head != request.Head {
		return errors.New("existing Onboarding Pull Request head differs from the deterministic approved commit")
	}
	if !pull.Found || strings.EqualFold(pull.State, "closed") {
		remoteBranch, found, readErr := a.Remote.OnboardingBranch(ctx, effect.Subject, request.Branch)
		if readErr != nil {
			return readErr
		}
		if found && remoteBranch.Head != request.Head {
			return errors.New("existing Onboarding branch ref differs from the deterministic approved commit")
		}
		if !found {
			if err := writer.Publish(ctx, prepared, effect.Subject, sourceURL, a.Credential); err != nil {
				return err
			}
		}
		pull, err = a.Remote.CreateOrUpdateOnboardingPull(ctx, request)
		if err != nil {
			return err
		}
	}
	decision, decisionErr = DecideOnboardingPull(pull, a.PlanDigest, request.Branch, base, baseHead)
	if decisionErr != nil || decision != PullDrift || pull.Head != request.Head || !strings.EqualFold(pull.State, "open") {
		return errors.New("created Onboarding Pull Request is not an exact unmerged approved state")
	}
	return nil
}

func (a *RepositoryAdapter) bindMergedOnboardingPull(ctx context.Context, effect setupcontract.Effect, request OnboardingPullRequest, pull PullReadback) error {
	decision, err := DecideOnboardingPull(pull, request.Digest, request.Branch, request.Base, request.BaseHead)
	if err != nil || decision != PullSatisfied {
		return errors.New("Onboarding Pull Request is not an exact merged approved state")
	}
	defaultBranch, err := a.Remote.DefaultBranchHead(ctx, request.Repository)
	if err != nil {
		return err
	}
	if defaultBranch.Name != request.Base || defaultBranch.Head != pull.MergeHead {
		return errors.New("GitHub default branch differs from the exact onboarding merge")
	}
	if err := a.Remote.VerifyContract(ctx, request.Repository, request.Base, effect.Parameters["manifest_digest"]); err != nil {
		return errors.New("managed Repository Contract differs after onboarding merge")
	}
	if a.MergeHeads == nil {
		a.MergeHeads = map[string]string{}
	}
	if old := a.MergeHeads[effect.ID]; old != "" && old != pull.MergeHead {
		return errors.New("Onboarding Pull Request merge head conflicts with persisted evidence")
	}
	a.MergeHeads[effect.ID] = pull.MergeHead
	return nil
}

// gitBranchWriter keeps the private GitWorkspace intact until its exact ref
// is published, then removes it through PreparedOnboardingBranch.Cleanup.
type gitBranchWriter struct{ temporaryRoot string }

func (w gitBranchWriter) Prepare(ctx context.Context, request OnboardingPullRequest, sourceURL string, credential GitCredential) (PreparedOnboardingBranch, error) {
	workspace, err := PrepareOnboardingCommit(ctx, request.Repository, sourceURL, request.BaseHead, w.temporaryRoot, request.Digest, request.Files, credential)
	if err != nil {
		return PreparedOnboardingBranch{}, err
	}
	return PreparedOnboardingBranch{
		Branch:  workspace.Branch,
		Head:    workspace.Head,
		Cleanup: workspace.Cleanup,
		publish: func(ctx context.Context, repository, sourceURL string, credential GitCredential) error {
			return PublishOnboardingBranch(ctx, workspace, repository, sourceURL, credential)
		},
	}, nil
}

func (w gitBranchWriter) Publish(ctx context.Context, prepared PreparedOnboardingBranch, repository, sourceURL string, credential GitCredential) error {
	if prepared.publish == nil {
		return errors.New("internal onboarding branch writer lost its prepared workspace")
	}
	return prepared.publish(ctx, repository, sourceURL, credential)
}

func (a *RepositoryAdapter) guardOnboardingMutation(repository string) error {
	owner, _, ok := strings.Cut(repository, "/")
	if !ok || owner == "" || a.Owner == "" || !strings.EqualFold(owner, a.Owner) {
		return errors.New("Onboarding Pull Request repository owner differs from the admitted owner binding")
	}
	if strings.TrimSpace(a.Credential.Token) == "" || strings.ContainsAny(a.Credential.Token, " \t\r\n") {
		return errors.New("Onboarding Pull Request mutation requires one scoped GitHub PAT")
	}
	return nil
}

func (a *RepositoryAdapter) canonicalOnboardingSource(repository, source string) (string, error) {
	canonical, err := GitHubHTTPSURL(repository)
	if err != nil || source != canonical {
		return "", errors.New("Onboarding Pull Request source differs from the canonical approved GitHub repository")
	}
	return canonical, nil
}

func decodeManagedFiles(raw string) (map[string][]byte, error) {
	encoded := map[string]string{}
	if err := json.Unmarshal([]byte(raw), &encoded); err != nil || len(encoded) == 0 {
		return nil, errors.New("Onboarding Pull Request managed files are invalid")
	}
	result := make(map[string][]byte, len(encoded))
	for path, value := range encoded {
		if strings.TrimSpace(path) == "" {
			return nil, errors.New("Onboarding Pull Request managed file path is empty")
		}
		data, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			return nil, fmt.Errorf("decode managed file %q: %w", path, err)
		}
		result[path] = data
	}
	return result, nil
}
