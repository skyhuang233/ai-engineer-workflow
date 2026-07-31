package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	githubapi "github.com/skyhuang233/workflow/internal/github"
)

type WorkerReleaseManifest struct {
	SchemaVersion      int    `json:"schema_version"`
	WorkerVersion      string `json:"worker_version"`
	SourceCommit       string `json:"source_commit"`
	Image              string `json:"image"`
	CodexVersion       string `json:"codex_version"`
	NoMistakesVersion  string `json:"no_mistakes_version"`
	NoMistakesCommit   string `json:"no_mistakes_commit"`
	GitHubActionsRunID int64  `json:"github_actions_run_id"`
}

type ReleaseFetcher struct {
	APIBase string
	HTTP    *http.Client
}

func (f ReleaseFetcher) Fetch(ctx context.Context, config Config, token string) (WorkerReleaseManifest, []byte, error) {
	client := githubapi.NewClient(f.APIBase, token, f.HTTP)
	tag := "worker-v" + config.Worker.Version
	var release struct {
		TargetCommitish string `json:"target_commitish"`
		Assets          []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"assets"`
	}
	releasePath := "/repos/" + config.Worker.ReleaseRepository + "/releases/tags/" + tag
	if err := client.RequestJSON(ctx, http.MethodGet, releasePath, nil, &release); err != nil {
		return WorkerReleaseManifest{}, nil, fmt.Errorf("read authoritative Worker Release: %w", err)
	}
	var assetID int64
	for _, asset := range release.Assets {
		if asset.Name == "worker-release.json" {
			assetID = asset.ID
			break
		}
	}
	if assetID == 0 {
		return WorkerReleaseManifest{}, nil, errors.New("authoritative Worker Release has no worker-release.json asset")
	}
	raw, err := client.RequestBytes(ctx,
		fmt.Sprintf("/repos/%s/releases/assets/%d", config.Worker.ReleaseRepository, assetID),
		"application/octet-stream")
	if err != nil {
		return WorkerReleaseManifest{}, nil, fmt.Errorf("download authoritative Worker Release Manifest: %w", err)
	}
	var manifest WorkerReleaseManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return WorkerReleaseManifest{}, nil, fmt.Errorf("decode authoritative Worker Release Manifest: %w", err)
	}
	if err := manifest.Validate(config); err != nil {
		return WorkerReleaseManifest{}, nil, err
	}
	if release.TargetCommitish != manifest.SourceCommit {
		return WorkerReleaseManifest{}, nil, errors.New("Worker Release target does not match manifest source commit")
	}
	var run struct {
		HeadSHA    string `json:"head_sha"`
		HeadBranch string `json:"head_branch"`
		Event      string `json:"event"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	}
	runPath := fmt.Sprintf("/repos/%s/actions/runs/%d", config.Worker.ReleaseRepository, manifest.GitHubActionsRunID)
	if err := client.RequestJSON(ctx, http.MethodGet, runPath, nil, &run); err != nil {
		return WorkerReleaseManifest{}, nil, fmt.Errorf("verify Worker publisher run: %w", err)
	}
	if run.HeadSHA != manifest.SourceCommit || run.HeadBranch != "main" || run.Event != "push" ||
		run.Status != "completed" || run.Conclusion != "success" {
		return WorkerReleaseManifest{}, nil, errors.New("Worker Release was not produced by a successful main push workflow")
	}
	return manifest, raw, nil
}

func (m WorkerReleaseManifest) Validate(config Config) error {
	switch {
	case m.SchemaVersion != 1:
		return errors.New("unsupported Worker Release Manifest schema")
	case m.WorkerVersion != config.Worker.Version:
		return errors.New("Worker Release version does not match toolchain")
	case !shaPattern.MatchString(m.SourceCommit):
		return errors.New("Worker Release source commit must be a full SHA")
	case !imagePattern.MatchString(m.Image) || !strings.HasPrefix(m.Image, config.Worker.ImageRepository+"@"):
		return errors.New("Worker Release image does not match the immutable toolchain repository")
	case m.CodexVersion != config.Codex.Version:
		return errors.New("Worker Release Codex version does not match toolchain")
	case m.NoMistakesVersion != config.NoMistakes.Version || m.NoMistakesCommit != config.NoMistakes.UpstreamCommit:
		return errors.New("Worker Release no-mistakes pin does not match toolchain")
	case m.GitHubActionsRunID <= 0:
		return errors.New("Worker Release Actions run ID is required")
	default:
		return nil
	}
}
