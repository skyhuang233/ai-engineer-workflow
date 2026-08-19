package doctor

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

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
	} `json:"head"`
	MergedBy struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"merged_by"`
}

type workflowTagRef struct {
	Object struct {
		Type string `json:"type"`
		SHA  string `json:"sha"`
	} `json:"object"`
}

func (r workflowTagRef) matchesDirectCommit(sourceCommit string) bool {
	return r.Object.Type == "commit" && r.Object.SHA == sourceCommit
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
		TargetCommitish string         `json:"target_commitish"`
		Draft           bool           `json:"draft"`
		Prerelease      bool           `json:"prerelease"`
		Immutable       bool           `json:"immutable"`
		Assets          []releaseAsset `json:"assets"`
	}
	if err := client.RequestJSON(ctx, http.MethodGet, "/repos/"+f.WorkflowRepository+"/releases/tags/"+tag, nil, &release); err != nil {
		return WorkflowReleaseManifest{}, nil, fmt.Errorf("read authoritative Workflow Release: %w", err)
	}
	if release.Draft || release.Prerelease || !release.Immutable {
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
	if manifest.Version != releaseConfig.Version || release.TargetCommitish != manifest.SourceCommit {
		return WorkflowReleaseManifest{}, nil, errors.New("Workflow Release tag, configuration, target, and manifest source do not agree")
	}
	var tagRef workflowTagRef
	if err := client.RequestJSON(ctx, http.MethodGet, "/repos/"+f.WorkflowRepository+"/git/ref/tags/"+tag, nil, &tagRef); err != nil {
		return WorkflowReleaseManifest{}, nil, fmt.Errorf("verify Workflow Release source tag: %w", err)
	}
	if !tagRef.matchesDirectCommit(manifest.SourceCommit) {
		return WorkflowReleaseManifest{}, nil, errors.New("Workflow Release tag must directly reference the manifest source commit")
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
	if err := verifyPublisher(ctx, client, f.WorkflowRepository, config, manifest); err != nil {
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

func verifyPublisher(ctx context.Context, client *githubapi.Client, repository string, config Config, manifest workflowrelease.Manifest) error {
	var workflow struct {
		ID    int64  `json:"id"`
		Path  string `json:"path"`
		State string `json:"state"`
	}
	if err := client.RequestJSON(ctx, http.MethodGet, "/repos/"+repository+"/actions/workflows/publish-workflow.yml", nil, &workflow); err != nil {
		return fmt.Errorf("verify Workflow publisher: %w", err)
	}
	var run struct {
		HeadSHA    string `json:"head_sha"`
		HeadBranch string `json:"head_branch"`
		Event      string `json:"event"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		WorkflowID int64  `json:"workflow_id"`
	}
	if err := client.RequestJSON(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/actions/runs/%d", repository, manifest.GitHubActionsRunID), nil, &run); err != nil {
		return fmt.Errorf("verify Workflow publisher run: %w", err)
	}
	if workflow.Path != ".github/workflows/publish-workflow.yml" || workflow.State != "active" || run.WorkflowID != workflow.ID || run.HeadSHA != manifest.SourceCommit || run.HeadBranch != "main" || run.Event != "push" || run.Status != "completed" || run.Conclusion != "success" {
		return errors.New("Workflow Release was not produced by the successful fixed main publisher")
	}
	var summaries []releasePullSummary
	if err := client.RequestJSON(ctx, http.MethodGet, "/repos/"+repository+"/commits/"+manifest.SourceCommit+"/pulls", nil, &summaries); err != nil {
		return err
	}
	matched := make([]releasePullSummary, 0, 1)
	for _, pull := range summaries {
		if pull.MergedAt != "" && pull.MergeCommitSHA == manifest.SourceCommit && pull.Base.Ref == "main" {
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
	if pull.MergedAt == "" || pull.MergeCommitSHA != manifest.SourceCommit || pull.Base.Ref != "main" || !branch || !strings.EqualFold(pull.MergedBy.Login, config.GitHub.Credential.Owner) || !strings.EqualFold(pull.MergedBy.Type, "user") || strings.HasSuffix(strings.ToLower(pull.MergedBy.Login), "[bot]") {
		return errors.New("Workflow Release source lacks an admitted owner merge")
	}
	return nil
}
