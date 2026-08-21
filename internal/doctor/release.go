package doctor

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	githubapi "github.com/skyhuang233/workflow/internal/github"
	"github.com/skyhuang233/workflow/internal/workflowrelease"
)

type WorkflowReleaseManifest = workflowrelease.Manifest

type ReleaseFetcher struct {
	APIBase            string
	HTTP               *http.Client
	WorkflowRepository string
}

type releaseAsset struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Digest string `json:"digest"`
}

type releasePullSummary struct {
	Number         int64  `json:"number"`
	MergedAt       string `json:"merged_at"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	Base           struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

type releasePull struct {
	MergedAt       string `json:"merged_at"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	Base           struct {
		Ref string `json:"ref"`
	} `json:"base"`
	Head struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	MergedBy struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"merged_by"`
}

type releaseCommitParent struct {
	SHA string `json:"sha"`
}

type releaseIntegrationCommit struct {
	Parents []releaseCommitParent `json:"parents"`
}

func (c releaseIntegrationCommit) containsExactPullHead(headSHA string) bool {
	if len(c.Parents) != 2 || headSHA == "" {
		return false
	}
	return c.Parents[0].SHA == headSHA || c.Parents[1].SHA == headSHA
}

type workflowTagRef struct {
	Object struct {
		Type string `json:"type"`
		SHA  string `json:"sha"`
	} `json:"object"`
}

type workflowTagObject struct {
	Tag     string `json:"tag"`
	Message string `json:"message"`
	Object  struct {
		Type string `json:"type"`
		SHA  string `json:"sha"`
	} `json:"object"`
}

var publisherTagMessagePattern = regexp.MustCompile(`^Workflow publisher provenance\nrun_id=([1-9][0-9]*)\nrun_attempt=([1-9][0-9]*)$`)

type releaseWorkflow struct {
	ID    int64  `json:"id"`
	Path  string `json:"path"`
	State string `json:"state"`
}

type releaseWorkflowRun struct {
	ID           int64                `json:"id"`
	HeadSHA      string               `json:"head_sha"`
	HeadBranch   string               `json:"head_branch"`
	Event        string               `json:"event"`
	Status       string               `json:"status"`
	Conclusion   string               `json:"conclusion"`
	WorkflowID   int64                `json:"workflow_id"`
	Path         string               `json:"path"`
	UpdatedAt    string               `json:"updated_at"`
	PullRequests []releasePullSummary `json:"pull_requests"`
	RunAttempt   int64                `json:"run_attempt"`
}

func completedNoLaterThan(completedAt, mergedAt string) bool {
	completed, err := time.Parse(time.RFC3339, completedAt)
	if err != nil {
		return false
	}
	merged, err := time.Parse(time.RFC3339, mergedAt)
	return err == nil && !completed.After(merged)
}

func (f ReleaseFetcher) Fetch(ctx context.Context, config Config, token string) (WorkflowReleaseManifest, []byte, error) {
	if !repoPattern.MatchString(f.WorkflowRepository) {
		return WorkflowReleaseManifest{}, nil, errors.New("workflow repository must be an owner/name")
	}
	if config.Worker.ReleaseRepository != f.WorkflowRepository {
		return WorkflowReleaseManifest{}, nil, errors.New("Workflow Release repository must match the workflow repository")
	}
	if err := githubapi.ValidateOwnerGuardedRepositoryName(f.WorkflowRepository, config.GitHub.Credential.Owner); err != nil {
		return WorkflowReleaseManifest{}, nil, errors.New("Workflow Release repository owner must match the configured owner")
	}
	client := githubapi.NewClient(f.APIBase, token, f.HTTP)
	var repository githubapi.RepositoryMetadata
	if err := client.RequestJSON(ctx, http.MethodGet, "/repos/"+f.WorkflowRepository, nil, &repository); err != nil {
		return WorkflowReleaseManifest{}, nil, fmt.Errorf("verify Workflow Release repository access: %w", err)
	}
	if err := repository.ValidateOwnerGuarded(f.WorkflowRepository, config.GitHub.Credential.Owner); err != nil {
		return WorkflowReleaseManifest{}, nil, fmt.Errorf("verify Workflow Release repository owner: %w", err)
	}

	releaseConfigRaw, err := client.RequestBytes(ctx, "/repos/"+f.WorkflowRepository+"/contents/config/workflow-release.json?ref=main", "application/vnd.github.raw+json")
	if err != nil {
		return WorkflowReleaseManifest{}, nil, fmt.Errorf("read current Workflow Release configuration: %w", err)
	}
	releaseConfig, err := workflowrelease.DecodeConfig(releaseConfigRaw)
	if err != nil {
		return WorkflowReleaseManifest{}, nil, err
	}
	tag := "workflow-v" + releaseConfig.Version
	var release struct {
		TagName    string         `json:"tag_name"`
		Body       string         `json:"body"`
		Draft      bool           `json:"draft"`
		Prerelease bool           `json:"prerelease"`
		Immutable  bool           `json:"immutable"`
		Assets     []releaseAsset `json:"assets"`
		Author     struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"author"`
	}
	if err := client.RequestJSON(ctx, http.MethodGet, "/repos/"+f.WorkflowRepository+"/releases/tags/"+tag, nil, &release); err != nil {
		return WorkflowReleaseManifest{}, nil, fmt.Errorf("read authoritative Workflow Release: %w", err)
	}
	if release.Draft || release.Prerelease || !release.Immutable || release.Author.Login != "github-actions[bot]" || release.Author.Type != "Bot" {
		return WorkflowReleaseManifest{}, nil, errors.New("authoritative Workflow Release must be published, stable, and immutable")
	}
	assets, err := exactWorkflowAssets(release.Assets)
	if err != nil {
		return WorkflowReleaseManifest{}, nil, err
	}

	download := func(name string) ([]byte, error) {
		asset := assets[name]
		raw, err := client.RequestBytes(ctx, fmt.Sprintf("/repos/%s/releases/assets/%d", f.WorkflowRepository, asset.ID), "application/octet-stream")
		if err != nil {
			return nil, fmt.Errorf("download %s: %w", name, err)
		}
		expected, err := workflowrelease.NormalizeSHA256(asset.Digest)
		if err != nil {
			return nil, fmt.Errorf("%s GitHub asset digest: %w", name, err)
		}
		got := fmt.Sprintf("%x", sha256.Sum256(raw))
		if got != expected {
			return nil, fmt.Errorf("%s bytes do not match GitHub asset metadata", name)
		}
		return raw, nil
	}
	manifestRaw, err := download(workflowrelease.ManifestAssetName)
	if err != nil {
		return WorkflowReleaseManifest{}, nil, err
	}
	manifest, err := workflowrelease.DecodeManifest(manifestRaw)
	if err != nil {
		return WorkflowReleaseManifest{}, nil, err
	}
	if manifest.Version != releaseConfig.Version || release.TagName != tag {
		return WorkflowReleaseManifest{}, nil, errors.New("Workflow Release tag and configuration do not agree")
	}
	var tagRef workflowTagRef
	if err := client.RequestJSON(ctx, http.MethodGet, "/repos/"+f.WorkflowRepository+"/git/ref/tags/"+tag, nil, &tagRef); err != nil {
		return WorkflowReleaseManifest{}, nil, fmt.Errorf("verify Workflow Release source tag: %w", err)
	}
	if tagRef.Object.Type != "tag" || !shaPattern.MatchString(tagRef.Object.SHA) {
		return WorkflowReleaseManifest{}, nil, errors.New("Workflow Release tag lacks annotated publisher provenance")
	}
	var tagObject workflowTagObject
	if err := client.RequestJSON(ctx, http.MethodGet, "/repos/"+f.WorkflowRepository+"/git/tags/"+tagRef.Object.SHA, nil, &tagObject); err != nil {
		return WorkflowReleaseManifest{}, nil, fmt.Errorf("verify Workflow Release tag provenance: %w", err)
	}
	provenance := publisherTagMessagePattern.FindStringSubmatch(tagObject.Message)
	if len(provenance) != 3 || tagObject.Tag != tag || tagObject.Object.Type != "commit" || !shaPattern.MatchString(tagObject.Object.SHA) {
		return WorkflowReleaseManifest{}, nil, errors.New("Workflow Release tag provenance is invalid")
	}
	var publisherRunID, publisherRunAttempt int64
	if _, err := fmt.Sscan(provenance[1], &publisherRunID); err != nil {
		return WorkflowReleaseManifest{}, nil, errors.New("Workflow Release publisher run provenance is invalid")
	}
	if _, err := fmt.Sscan(provenance[2], &publisherRunAttempt); err != nil {
		return WorkflowReleaseManifest{}, nil, errors.New("Workflow Release publisher attempt provenance is invalid")
	}
	wantBody := fmt.Sprintf("Immutable atomic Agent Workflow release.\n\nPublisher Run: %d\nPublisher Attempt: %d", publisherRunID, publisherRunAttempt)
	if release.Body != wantBody {
		return WorkflowReleaseManifest{}, nil, errors.New("Workflow Release body differs from its annotated publisher provenance")
	}
	bundle, err := download(workflowrelease.BundleAssetName)
	if err != nil {
		return WorkflowReleaseManifest{}, nil, err
	}
	if fmt.Sprintf("%x", sha256.Sum256(bundle)) != manifest.Bundle.SHA256 {
		return WorkflowReleaseManifest{}, nil, errors.New("Workflow Bundle checksum does not match the manifest")
	}
	sbom, err := download(workflowrelease.SBOMAssetName)
	if err != nil {
		return WorkflowReleaseManifest{}, nil, err
	}
	if err := validateWorkerSBOM(sbom); err != nil {
		return WorkflowReleaseManifest{}, nil, err
	}
	if fmt.Sprintf("%x", sha256.Sum256(sbom)) != manifest.SBOM.SHA256 {
		return WorkflowReleaseManifest{}, nil, errors.New("Worker SBOM checksum does not match the manifest")
	}
	if manifest.Worker.Image != config.Worker.ImageRepository+"@sha256:"+strings.TrimPrefix(manifest.Worker.Image, config.Worker.ImageRepository+"@sha256:") {
		return WorkflowReleaseManifest{}, nil, errors.New("Workflow Release image does not match the configured repository")
	}
	if err := verifyPublisher(ctx, client, f.WorkflowRepository, config, manifest, tagObject.Object.SHA, publisherRunID, publisherRunAttempt); err != nil {
		return WorkflowReleaseManifest{}, nil, err
	}
	return manifest, manifestRaw, nil
}

func exactWorkflowAssets(input []releaseAsset) (map[string]releaseAsset, error) {
	if len(input) != 3 {
		return nil, errors.New("Workflow Release must contain exactly three assets")
	}
	want := map[string]bool{workflowrelease.BundleAssetName: true, workflowrelease.ManifestAssetName: true, workflowrelease.SBOMAssetName: true}
	result := make(map[string]releaseAsset, 3)
	for _, asset := range input {
		if !want[asset.Name] || asset.ID <= 0 || result[asset.Name].ID != 0 {
			return nil, errors.New("Workflow Release assets are missing, duplicated, or unexpected")
		}
		result[asset.Name] = asset
	}
	return result, nil
}

func validateWorkerSBOM(raw []byte) error {
	var document struct {
		SPDXVersion string `json:"spdxVersion"`
		Name        string `json:"name"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		return fmt.Errorf("decode authoritative Worker SBOM: %w", err)
	}
	if document.SPDXVersion != "SPDX-2.3" || strings.TrimSpace(document.Name) == "" {
		return errors.New("authoritative Worker SBOM must be a named SPDX 2.3 document")
	}
	return nil
}

func verifyPublisher(ctx context.Context, client *githubapi.Client, repository string, config Config, manifest workflowrelease.Manifest, mergeCommit string, publisherRunID, publisherRunAttempt int64) error {
	var summaries []releasePullSummary
	if err := client.RequestJSON(ctx, http.MethodGet, "/repos/"+repository+"/commits/"+mergeCommit+"/pulls", nil, &summaries); err != nil {
		return err
	}
	matched := make([]releasePullSummary, 0, 1)
	for _, pull := range summaries {
		if pull.MergedAt != "" && pull.MergeCommitSHA == mergeCommit && pull.Base.Ref == "main" {
			matched = append(matched, pull)
		}
	}
	if len(matched) != 1 || matched[0].Number <= 0 {
		return errors.New("Workflow Release source lacks one owner-merged pull request")
	}
	var pull releasePull
	if err := client.RequestJSON(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d", repository, matched[0].Number), nil, &pull); err != nil {
		return err
	}
	branch := pull.Head.Ref == "release-"+manifest.Version || pull.Head.Ref == "hotfix-"+manifest.Version
	if pull.MergedAt == "" || pull.MergeCommitSHA != mergeCommit || pull.Base.Ref != "main" || pull.Head.SHA != manifest.CandidateSourceCommit || !branch || !strings.EqualFold(pull.MergedBy.Login, config.GitHub.Credential.Owner) || !strings.EqualFold(pull.MergedBy.Type, "user") || strings.HasSuffix(strings.ToLower(pull.MergedBy.Login), "[bot]") {
		return errors.New("Workflow Release source lacks an admitted owner merge")
	}
	var integration releaseIntegrationCommit
	if err := client.RequestJSON(ctx, http.MethodGet, "/repos/"+repository+"/git/commits/"+mergeCommit, nil, &integration); err != nil {
		return fmt.Errorf("verify Workflow Release integration commit: %w", err)
	}
	if !integration.containsExactPullHead(pull.Head.SHA) {
		return errors.New("Workflow Release source is not a two-parent merge containing the exact pull request head")
	}
	var qualificationWorkflow releaseWorkflow
	if err := client.RequestJSON(ctx, http.MethodGet, "/repos/"+repository+"/actions/workflows/worker-contract.yml", nil, &qualificationWorkflow); err != nil {
		return fmt.Errorf("verify qualification workflow: %w", err)
	}
	var qualificationRun releaseWorkflowRun
	if err := client.RequestJSON(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/actions/runs/%d/attempts/%d", repository, manifest.QualificationRunID, manifest.QualificationRunAttempt), nil, &qualificationRun); err != nil {
		return fmt.Errorf("verify qualification run: %w", err)
	}
	qualifiedPull := false
	for _, associated := range qualificationRun.PullRequests {
		if associated.Number == matched[0].Number {
			qualifiedPull = true
		}
	}
	if qualificationWorkflow.Path != ".github/workflows/worker-contract.yml" || qualificationWorkflow.State != "active" || qualificationRun.WorkflowID != qualificationWorkflow.ID || qualificationRun.Path != qualificationWorkflow.Path || qualificationRun.RunAttempt != manifest.QualificationRunAttempt || qualificationRun.HeadSHA != manifest.CandidateSourceCommit || qualificationRun.Event != "pull_request" || qualificationRun.Status != "completed" || qualificationRun.Conclusion != "success" || !qualifiedPull {
		return errors.New("Workflow Release candidate lacks authoritative successful qualification provenance")
	}
	if !completedNoLaterThan(qualificationRun.UpdatedAt, pull.MergedAt) {
		return errors.New("Workflow Release qualification did not complete before the owner merge")
	}
	var publisherWorkflow releaseWorkflow
	if err := client.RequestJSON(ctx, http.MethodGet, "/repos/"+repository+"/actions/workflows/publish-workflow.yml", nil, &publisherWorkflow); err != nil {
		return fmt.Errorf("verify Workflow publisher: %w", err)
	}
	var publisherRun releaseWorkflowRun
	if err := client.RequestJSON(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/actions/runs/%d", repository, publisherRunID), nil, &publisherRun); err != nil {
		return fmt.Errorf("verify Workflow publisher run: %w", err)
	}
	if publisherWorkflow.Path == ".github/workflows/publish-workflow.yml" && publisherWorkflow.State == "active" && publisherRun.ID == publisherRunID && publisherRun.RunAttempt >= publisherRunAttempt && publisherRun.WorkflowID == publisherWorkflow.ID && publisherRun.Path == publisherWorkflow.Path && publisherRun.HeadSHA == mergeCommit && publisherRun.HeadBranch == "main" && publisherRun.Event == "push" && publisherRun.Status == "completed" && publisherRun.Conclusion == "success" {
		return nil
	}
	return errors.New("Workflow Release provenance is not its exact successful fixed main publisher run")
}
