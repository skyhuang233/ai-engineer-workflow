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
	SchemaVersion                int    `json:"schema_version"`
	WorkerVersion                string `json:"worker_version"`
	SourceCommit                 string `json:"source_commit"`
	Image                        string `json:"image"`
	CodexVersion                 string `json:"codex_version"`
	NoMistakesVersion            string `json:"no_mistakes_version"`
	NoMistakesUpstreamRepository string `json:"no_mistakes_upstream_repository"`
	NoMistakesCommit             string `json:"no_mistakes_commit"`
	NoMistakesForkRepository     string `json:"no_mistakes_fork_repository"`
	NoMistakesForkRelease        string `json:"no_mistakes_fork_release"`
	NoMistakesLinuxAMD64SHA256   string `json:"no_mistakes_linux_amd64_sha256"`
	GitHubActionsRunID           int64  `json:"github_actions_run_id"`
}

type ReleaseFetcher struct {
	APIBase string
	HTTP    *http.Client
}

func (f ReleaseFetcher) Fetch(ctx context.Context, config Config, token string) (WorkerReleaseManifest, []byte, error) {
	client := githubapi.NewClient(f.APIBase, token, f.HTTP)
	var repository struct {
		Private bool `json:"private"`
	}
	if err := client.RequestJSON(ctx, http.MethodGet, "/repos/"+config.Worker.ReleaseRepository, nil, &repository); err != nil {
		return WorkerReleaseManifest{}, nil, fmt.Errorf("verify Worker Release repository visibility: %w", err)
	}
	if repository.Private {
		return WorkerReleaseManifest{}, nil, errors.New("Worker Release repository must be public")
	}
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
	manifestAssets := 0
	for _, asset := range release.Assets {
		if asset.Name == "worker-release.json" {
			manifestAssets++
			assetID = asset.ID
		}
	}
	if manifestAssets != 1 || assetID == 0 {
		return WorkerReleaseManifest{}, nil, errors.New("authoritative Worker Release must have exactly one worker-release.json asset")
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
		WorkflowID int64  `json:"workflow_id"`
	}
	var workflow struct {
		ID    int64  `json:"id"`
		Path  string `json:"path"`
		State string `json:"state"`
	}
	workflowPath := "/repos/" + config.Worker.ReleaseRepository + "/actions/workflows/publish-worker.yml"
	if err := client.RequestJSON(ctx, http.MethodGet, workflowPath, nil, &workflow); err != nil {
		return WorkerReleaseManifest{}, nil, fmt.Errorf("verify Worker publisher workflow: %w", err)
	}
	runPath := fmt.Sprintf("/repos/%s/actions/runs/%d", config.Worker.ReleaseRepository, manifest.GitHubActionsRunID)
	if err := client.RequestJSON(ctx, http.MethodGet, runPath, nil, &run); err != nil {
		return WorkerReleaseManifest{}, nil, fmt.Errorf("verify Worker publisher run: %w", err)
	}
	if run.HeadSHA != manifest.SourceCommit || run.HeadBranch != "main" || run.Event != "push" ||
		run.Status != "completed" || run.Conclusion != "success" || run.WorkflowID != workflow.ID ||
		workflow.Path != ".github/workflows/publish-worker.yml" || workflow.State != "active" {
		return WorkerReleaseManifest{}, nil, errors.New("Worker Release was not produced by a successful main push workflow")
	}
	var main struct {
		SHA string `json:"sha"`
	}
	if err := client.RequestJSON(ctx, http.MethodGet, "/repos/"+config.Worker.ReleaseRepository+"/commits/main", nil, &main); err != nil {
		return WorkerReleaseManifest{}, nil, fmt.Errorf("resolve current main commit: %w", err)
	}
	if main.SHA != manifest.SourceCommit {
		return WorkerReleaseManifest{}, nil, errors.New("Worker Release source commit is not current main")
	}
	var pulls []struct {
		MergedAt       string `json:"merged_at"`
		MergeCommitSHA string `json:"merge_commit_sha"`
		Base           struct {
			Ref string `json:"ref"`
		} `json:"base"`
		MergedBy struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"merged_by"`
	}
	if err := client.RequestJSON(ctx, http.MethodGet, "/repos/"+config.Worker.ReleaseRepository+"/commits/"+manifest.SourceCommit+"/pulls", nil, &pulls); err != nil {
		return WorkerReleaseManifest{}, nil, fmt.Errorf("verify Worker Release merge provenance: %w", err)
	}
	matched := 0
	for _, pull := range pulls {
		if pull.MergedAt != "" && pull.MergeCommitSHA == manifest.SourceCommit && pull.Base.Ref == "main" &&
			strings.EqualFold(pull.MergedBy.Login, config.GitHub.Credential.Owner) && !strings.EqualFold(pull.MergedBy.Type, "bot") && !strings.HasSuffix(strings.ToLower(pull.MergedBy.Login), "[bot]") {
			matched++
		}
	}
	if matched != 1 {
		return WorkerReleaseManifest{}, nil, errors.New("Worker Release source commit lacks an unambiguous owner-merged pull request")
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
	case m.NoMistakesVersion != config.NoMistakes.Version || m.NoMistakesUpstreamRepository != config.NoMistakes.UpstreamRepository ||
		m.NoMistakesCommit != config.NoMistakes.UpstreamCommit ||
		m.NoMistakesForkRepository != config.NoMistakes.ForkRepository || m.NoMistakesForkRelease != config.NoMistakes.ForkRelease ||
		m.NoMistakesLinuxAMD64SHA256 != config.NoMistakes.LinuxAMD64SHA256:
		return errors.New("Worker Release no-mistakes pin does not match toolchain")
	case m.GitHubActionsRunID <= 0:
		return errors.New("Worker Release Actions run ID is required")
	default:
		return nil
	}
}
