package doctor

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
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
	BuildInputIdentity           string `json:"build_input_identity"`
	GitHubActionsRunID           int64  `json:"github_actions_run_id"`
}

type workerBuildInputs struct {
	SchemaVersion             int           `json:"schema_version"`
	DeployWorkerTree          string        `json:"deploy_worker_tree"`
	PublishWorkerWorkflowBlob string        `json:"publish_worker_workflow_blob"`
	Codex                     ToolPin       `json:"codex"`
	NoMistakes                NoMistakesPin `json:"no_mistakes"`
	Worker                    WorkerPin     `json:"worker"`
}

type canonicalWorkerBuildInputs struct {
	SchemaVersion             int           `json:"schema_version"`
	DeployWorkerTree          string        `json:"deploy_worker_tree"`
	PublishWorkerWorkflowBlob string        `json:"publish_worker_workflow_blob"`
	Codex                     ToolPin       `json:"codex"`
	NoMistakes                NoMistakesPin `json:"no_mistakes"`
	Worker                    WorkerPin     `json:"worker"`
}

type resolvedWorkerBuildInputs struct {
	CommitSHA string
	Config    Config
	Identity  string
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
	currentInputs, err := resolveWorkerBuildInputs(ctx, client, config.Worker.ReleaseRepository, "main")
	if err != nil {
		return WorkerReleaseManifest{}, nil, fmt.Errorf("resolve current Worker build inputs: %w", err)
	}
	tag := workerReleaseTag(currentInputs.Config.Worker.Version, currentInputs.Identity)
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
	if len(release.Assets) != 1 || manifestAssets != 1 || assetID == 0 {
		return WorkerReleaseManifest{}, nil, errors.New("authoritative Worker Release must contain only one worker-release.json asset")
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
	sourceInputs, err := resolveWorkerBuildInputs(ctx, client, config.Worker.ReleaseRepository, manifest.SourceCommit)
	if err != nil {
		return WorkerReleaseManifest{}, nil, fmt.Errorf("resolve Worker Release source build inputs: %w", err)
	}
	if sourceInputs.CommitSHA != manifest.SourceCommit || sourceInputs.Identity != manifest.BuildInputIdentity {
		return WorkerReleaseManifest{}, nil, errors.New("Worker Release manifest does not match its source build inputs")
	}
	if currentInputs.Identity != manifest.BuildInputIdentity {
		return WorkerReleaseManifest{}, nil, errors.New("current main Worker build inputs do not match the Worker Release manifest")
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
	case m.SchemaVersion != 2:
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
	case !sha256Pattern.MatchString(m.BuildInputIdentity):
		return errors.New("Worker Release build input identity must be SHA-256")
	case m.GitHubActionsRunID <= 0:
		return errors.New("Worker Release Actions run ID is required")
	default:
		return nil
	}
}

func resolveWorkerBuildInputs(ctx context.Context, client *githubapi.Client, repository, ref string) (resolvedWorkerBuildInputs, error) {
	var commit struct {
		SHA    string `json:"sha"`
		Commit struct {
			Tree struct {
				SHA string `json:"sha"`
			} `json:"tree"`
		} `json:"commit"`
	}
	if err := client.RequestJSON(ctx, http.MethodGet, "/repos/"+repository+"/commits/"+ref, nil, &commit); err != nil {
		return resolvedWorkerBuildInputs{}, fmt.Errorf("resolve commit %q: %w", ref, err)
	}
	if !shaPattern.MatchString(commit.SHA) || !shaPattern.MatchString(commit.Commit.Tree.SHA) {
		return resolvedWorkerBuildInputs{}, errors.New("Worker build input commit has an invalid Git object identity")
	}
	configData, err := client.RequestBytes(ctx, "/repos/"+repository+"/contents/config/toolchain.json?ref="+ref, "application/vnd.github.raw+json")
	if err != nil {
		return resolvedWorkerBuildInputs{}, fmt.Errorf("read toolchain config at %q: %w", ref, err)
	}
	var config Config
	decoder := json.NewDecoder(strings.NewReader(string(configData)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return resolvedWorkerBuildInputs{}, fmt.Errorf("decode toolchain config at %q: %w", ref, err)
	}
	if err := config.Validate(); err != nil {
		return resolvedWorkerBuildInputs{}, fmt.Errorf("validate toolchain config at %q: %w", ref, err)
	}
	deployTree, err := gitTreeEntry(ctx, client, repository, commit.Commit.Tree.SHA, "deploy", "tree")
	if err != nil {
		return resolvedWorkerBuildInputs{}, err
	}
	workerTree, err := gitTreeEntry(ctx, client, repository, deployTree, "worker", "tree")
	if err != nil {
		return resolvedWorkerBuildInputs{}, err
	}
	githubTree, err := gitTreeEntry(ctx, client, repository, commit.Commit.Tree.SHA, ".github", "tree")
	if err != nil {
		return resolvedWorkerBuildInputs{}, err
	}
	workflowsTree, err := gitTreeEntry(ctx, client, repository, githubTree, "workflows", "tree")
	if err != nil {
		return resolvedWorkerBuildInputs{}, err
	}
	publisherWorkflow, err := gitTreeEntry(ctx, client, repository, workflowsTree, "publish-worker.yml", "blob")
	if err != nil {
		return resolvedWorkerBuildInputs{}, err
	}
	return resolvedWorkerBuildInputs{CommitSHA: commit.SHA, Config: config, Identity: workerBuildInputIdentity(config, workerTree, publisherWorkflow)}, nil
}

func gitTreeEntry(ctx context.Context, client *githubapi.Client, repository, tree, path, objectType string) (string, error) {
	var response struct {
		Tree []struct {
			Path string `json:"path"`
			Type string `json:"type"`
			SHA  string `json:"sha"`
		} `json:"tree"`
	}
	if err := client.RequestJSON(ctx, http.MethodGet, "/repos/"+repository+"/git/trees/"+tree, nil, &response); err != nil {
		return "", fmt.Errorf("read Git tree %q: %w", tree, err)
	}
	for _, entry := range response.Tree {
		if entry.Path == path && entry.Type == objectType && shaPattern.MatchString(entry.SHA) {
			return entry.SHA, nil
		}
	}
	return "", fmt.Errorf("Git tree %q lacks %s %q", tree, objectType, path)
}

func workerBuildInputIdentity(config Config, workerTree, publisherWorkflow string) string {
	inputs := workerBuildInputs{
		SchemaVersion:             2,
		DeployWorkerTree:          workerTree,
		PublishWorkerWorkflowBlob: publisherWorkflow,
		Codex:                     config.Codex,
		NoMistakes:                config.NoMistakes,
		Worker:                    config.Worker,
	}
	encoded, _ := json.Marshal(canonicalizeWorkerBuildInputs(inputs))
	digest := sha256.Sum256(encoded)
	return fmt.Sprintf("%x", digest)
}

func canonicalizeWorkerBuildInputs(inputs workerBuildInputs) canonicalWorkerBuildInputs {
	return canonicalWorkerBuildInputs{
		SchemaVersion:             inputs.SchemaVersion,
		DeployWorkerTree:          base64.StdEncoding.EncodeToString([]byte(inputs.DeployWorkerTree)),
		PublishWorkerWorkflowBlob: base64.StdEncoding.EncodeToString([]byte(inputs.PublishWorkerWorkflowBlob)),
		Codex: ToolPin{
			Version: base64.StdEncoding.EncodeToString([]byte(inputs.Codex.Version)),
		},
		NoMistakes: NoMistakesPin{
			Version:            base64.StdEncoding.EncodeToString([]byte(inputs.NoMistakes.Version)),
			UpstreamRepository: base64.StdEncoding.EncodeToString([]byte(inputs.NoMistakes.UpstreamRepository)),
			UpstreamCommit:     base64.StdEncoding.EncodeToString([]byte(inputs.NoMistakes.UpstreamCommit)),
			ForkRepository:     base64.StdEncoding.EncodeToString([]byte(inputs.NoMistakes.ForkRepository)),
			ForkRelease:        base64.StdEncoding.EncodeToString([]byte(inputs.NoMistakes.ForkRelease)),
			LinuxAMD64SHA256:   base64.StdEncoding.EncodeToString([]byte(inputs.NoMistakes.LinuxAMD64SHA256)),
		},
		Worker: WorkerPin{
			Version:           base64.StdEncoding.EncodeToString([]byte(inputs.Worker.Version)),
			ImageRepository:   base64.StdEncoding.EncodeToString([]byte(inputs.Worker.ImageRepository)),
			ReleaseRepository: base64.StdEncoding.EncodeToString([]byte(inputs.Worker.ReleaseRepository)),
		},
	}
}

func workerReleaseTag(workerVersion, buildInputIdentity string) string {
	return "worker-v" + workerVersion + "-" + buildInputIdentity
}
