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
	"github.com/skyhuang233/workflow/internal/workerrelease"
)

type WorkerReleaseManifest struct {
	workerrelease.ToolProvenance
	SchemaVersion                int                     `json:"schema_version"`
	WorkerVersion                string                  `json:"worker_version"`
	SourceCommit                 string                  `json:"source_commit"`
	Image                        string                  `json:"image"`
	GitHubCLILinuxAMD64SHA256    string                  `json:"github_cli_linux_amd64_sha256"`
	GoLinuxAMD64SHA256           string                  `json:"go_linux_amd64_sha256"`
	NoMistakesUpstreamRepository string                  `json:"no_mistakes_upstream_repository"`
	NoMistakesUpstreamCommit     string                  `json:"no_mistakes_upstream_commit"`
	NoMistakesForkRepository     string                  `json:"no_mistakes_fork_repository"`
	NoMistakesForkCommit         string                  `json:"no_mistakes_fork_commit"`
	NoMistakesForkRelease        string                  `json:"no_mistakes_fork_release"`
	NoMistakesLinuxAMD64SHA256   string                  `json:"no_mistakes_linux_amd64_sha256"`
	BuildInputIdentity           string                  `json:"build_input_identity"`
	SBOMSHA256                   string                  `json:"sbom_sha256"`
	VulnerabilityScan            VulnerabilityScanPolicy `json:"vulnerability_scan"`
	GitHubActionsRunID           int64                   `json:"github_actions_run_id"`
}

type VulnerabilityScanPolicy struct {
	Scanner        string `json:"scanner"`
	SeverityCutoff string `json:"severity_cutoff"`
	OnlyFixed      bool   `json:"only_fixed"`
}

type workerBuildInputs struct {
	SchemaVersion                   int           `json:"schema_version"`
	DeployWorkerTree                string        `json:"deploy_worker_tree"`
	DeliverySourceDigestCommandTree string        `json:"delivery_source_digest_command_tree"`
	DeliverySourceDigestPackageTree string        `json:"delivery_source_digest_package_tree"`
	GoModBlob                       string        `json:"go_mod_blob"`
	GoSumBlob                       string        `json:"go_sum_blob"`
	PublishWorkerWorkflowBlob       string        `json:"publish_worker_workflow_blob"`
	Codex                           ToolPin       `json:"codex"`
	GitHubCLI                       GitHubCLIPin  `json:"github_cli"`
	Go                              GoPin         `json:"go"`
	NoMistakes                      NoMistakesPin `json:"no_mistakes"`
	Worker                          WorkerPin     `json:"worker"`
}

type canonicalWorkerBuildInputs struct {
	SchemaVersion                   int           `json:"schema_version"`
	DeployWorkerTree                string        `json:"deploy_worker_tree"`
	DeliverySourceDigestCommandTree string        `json:"delivery_source_digest_command_tree"`
	DeliverySourceDigestPackageTree string        `json:"delivery_source_digest_package_tree"`
	GoModBlob                       string        `json:"go_mod_blob"`
	GoSumBlob                       string        `json:"go_sum_blob"`
	PublishWorkerWorkflowBlob       string        `json:"publish_worker_workflow_blob"`
	Codex                           ToolPin       `json:"codex"`
	GitHubCLI                       GitHubCLIPin  `json:"github_cli"`
	Go                              GoPin         `json:"go"`
	NoMistakes                      NoMistakesPin `json:"no_mistakes"`
	Worker                          WorkerPin     `json:"worker"`
}

type resolvedWorkerBuildInputs struct {
	CommitSHA string
	Config    Config
	Identity  string
}

type ReleaseFetcher struct {
	APIBase            string
	HTTP               *http.Client
	WorkflowRepository string
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
	MergedBy struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"merged_by"`
}

func (f ReleaseFetcher) Fetch(ctx context.Context, config Config, token string) (WorkerReleaseManifest, []byte, error) {
	if !repoPattern.MatchString(f.WorkflowRepository) {
		return WorkerReleaseManifest{}, nil, errors.New("workflow repository must be an owner/name")
	}
	if config.Worker.ReleaseRepository != f.WorkflowRepository {
		return WorkerReleaseManifest{}, nil, errors.New("Worker Release repository must match the workflow repository")
	}
	if err := githubapi.ValidateOwnerGuardedRepositoryName(config.Worker.ReleaseRepository, config.GitHub.Credential.Owner); err != nil {
		return WorkerReleaseManifest{}, nil, errors.New("Worker Release repository owner must match the configured owner")
	}
	client := githubapi.NewClient(f.APIBase, token, f.HTTP)
	var repository githubapi.RepositoryMetadata
	if err := client.RequestJSON(ctx, http.MethodGet, "/repos/"+config.Worker.ReleaseRepository, nil, &repository); err != nil {
		return WorkerReleaseManifest{}, nil, fmt.Errorf("verify Worker Release repository access: %w", err)
	}
	if err := repository.ValidateOwnerGuarded(config.Worker.ReleaseRepository, config.GitHub.Credential.Owner); err != nil {
		return WorkerReleaseManifest{}, nil, fmt.Errorf("verify Worker Release repository owner: %w", err)
	}
	currentInputs, err := resolveWorkerBuildInputs(ctx, client, config.Worker.ReleaseRepository, "main")
	if err != nil {
		return WorkerReleaseManifest{}, nil, fmt.Errorf("resolve current Worker build inputs: %w", err)
	}
	tag := workerReleaseTag(currentInputs.Config.Worker.Version, currentInputs.Identity)
	var release struct {
		TargetCommitish string `json:"target_commitish"`
		Immutable       bool   `json:"immutable"`
		Assets          []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"assets"`
	}
	releasePath := "/repos/" + config.Worker.ReleaseRepository + "/releases/tags/" + tag
	if err := client.RequestJSON(ctx, http.MethodGet, releasePath, nil, &release); err != nil {
		return WorkerReleaseManifest{}, nil, fmt.Errorf("read authoritative Worker Release: %w", err)
	}
	if !release.Immutable {
		return WorkerReleaseManifest{}, nil, errors.New("authoritative Worker Release must be immutable")
	}
	var manifestAssetID, sbomAssetID int64
	manifestAssets, sbomAssets := 0, 0
	for _, asset := range release.Assets {
		switch asset.Name {
		case "worker-release.json":
			manifestAssets++
			manifestAssetID = asset.ID
		case "worker-sbom.spdx.json":
			sbomAssets++
			sbomAssetID = asset.ID
		}
	}
	if len(release.Assets) != 2 || manifestAssets != 1 || sbomAssets != 1 || manifestAssetID == 0 || sbomAssetID == 0 {
		return WorkerReleaseManifest{}, nil, errors.New("authoritative Worker Release must contain exactly one worker-release.json and one worker-sbom.spdx.json asset")
	}
	raw, err := client.RequestBytes(ctx,
		fmt.Sprintf("/repos/%s/releases/assets/%d", config.Worker.ReleaseRepository, manifestAssetID),
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
	sbom, err := client.RequestBytes(ctx,
		fmt.Sprintf("/repos/%s/releases/assets/%d", config.Worker.ReleaseRepository, sbomAssetID),
		"application/octet-stream")
	if err != nil {
		return WorkerReleaseManifest{}, nil, fmt.Errorf("download authoritative Worker SBOM: %w", err)
	}
	if err := validateWorkerSBOM(sbom); err != nil {
		return WorkerReleaseManifest{}, nil, err
	}
	sbomDigest := sha256.Sum256(sbom)
	if fmt.Sprintf("%x", sbomDigest) != manifest.SBOMSHA256 {
		return WorkerReleaseManifest{}, nil, errors.New("Worker SBOM checksum does not match the Release Manifest")
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
	var pulls []releasePullSummary
	if err := client.RequestJSON(ctx, http.MethodGet, "/repos/"+config.Worker.ReleaseRepository+"/commits/"+manifest.SourceCommit+"/pulls", nil, &pulls); err != nil {
		return WorkerReleaseManifest{}, nil, fmt.Errorf("verify Worker Release merge provenance: %w", err)
	}
	matched := make([]releasePullSummary, 0, 1)
	for _, pull := range pulls {
		if pull.MergedAt != "" && pull.MergeCommitSHA == manifest.SourceCommit && pull.Base.Ref == "main" {
			matched = append(matched, pull)
		}
	}
	if len(matched) != 1 || matched[0].Number <= 0 {
		return WorkerReleaseManifest{}, nil, errors.New("Worker Release source commit lacks an unambiguous owner-merged pull request")
	}
	var pull releasePull
	pullPath := fmt.Sprintf("/repos/%s/pulls/%d", config.Worker.ReleaseRepository, matched[0].Number)
	if err := client.RequestJSON(ctx, http.MethodGet, pullPath, nil, &pull); err != nil {
		return WorkerReleaseManifest{}, nil, fmt.Errorf("read Worker Release merge provenance pull request: %w", err)
	}
	if pull.MergedAt == "" || pull.MergeCommitSHA != manifest.SourceCommit || pull.Base.Ref != "main" ||
		!strings.EqualFold(pull.MergedBy.Login, config.GitHub.Credential.Owner) || !strings.EqualFold(pull.MergedBy.Type, "user") || strings.HasSuffix(strings.ToLower(pull.MergedBy.Login), "[bot]") {
		return WorkerReleaseManifest{}, nil, errors.New("Worker Release source commit lacks an unambiguous owner-merged pull request")
	}
	return manifest, raw, nil
}

func (m WorkerReleaseManifest) Validate(config Config) error {
	if _, err := m.ToolVersions(); err != nil {
		return err
	}
	switch {
	case m.SchemaVersion != 6:
		return errors.New("unsupported Worker Release Manifest schema")
	case m.WorkerVersion != config.Worker.Version:
		return errors.New("Worker Release version does not match toolchain")
	case !shaPattern.MatchString(m.SourceCommit):
		return errors.New("Worker Release source commit must be a full SHA")
	case !imagePattern.MatchString(m.Image) || !strings.HasPrefix(m.Image, config.Worker.ImageRepository+"@"):
		return errors.New("Worker Release image does not match the immutable toolchain repository")
	case m.CodexVersion != config.Codex.Version:
		return errors.New("Worker Release Codex version does not match toolchain")
	case m.GitHubCLIVersion != config.GitHubCLI.Version || m.GitHubCLILinuxAMD64SHA256 != config.GitHubCLI.LinuxAMD64SHA256:
		return errors.New("Worker Release GitHub CLI pin does not match toolchain")
	case m.GoVersion != config.Go.Version || m.GoLinuxAMD64SHA256 != config.Go.LinuxAMD64SHA256:
		return errors.New("Worker Release Go pin does not match toolchain")
	case m.NoMistakesVersion != config.NoMistakes.Version || m.NoMistakesUpstreamRepository != config.NoMistakes.UpstreamRepository ||
		m.NoMistakesUpstreamCommit != config.NoMistakes.UpstreamCommit ||
		m.NoMistakesForkRepository != config.NoMistakes.ForkRepository || m.NoMistakesForkRelease != config.NoMistakes.ForkRelease ||
		m.NoMistakesForkCommit != config.NoMistakes.ForkCommit ||
		m.NoMistakesLinuxAMD64SHA256 != config.NoMistakes.LinuxAMD64SHA256:
		return errors.New("Worker Release no-mistakes pin does not match toolchain")
	case !sha256Pattern.MatchString(m.BuildInputIdentity):
		return errors.New("Worker Release build input identity must be SHA-256")
	case !sha256Pattern.MatchString(m.SBOMSHA256):
		return errors.New("Worker Release SBOM checksum must be SHA-256")
	case m.VulnerabilityScan.Scanner != "grype" || m.VulnerabilityScan.SeverityCutoff != "high" || !m.VulnerabilityScan.OnlyFixed:
		return errors.New("Worker Release vulnerability scan must fail on fixable high-or-greater Grype findings")
	case m.GitHubActionsRunID <= 0:
		return errors.New("Worker Release Actions run ID is required")
	default:
		return nil
	}
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
	if err := config.validateWorkerBuildInputs(); err != nil {
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
	cmdTree, err := gitTreeEntry(ctx, client, repository, commit.Commit.Tree.SHA, "cmd", "tree")
	if err != nil {
		return resolvedWorkerBuildInputs{}, err
	}
	deliverySourceDigestCommandTree, err := gitTreeEntry(ctx, client, repository, cmdTree, "delivery-source-digest", "tree")
	if err != nil {
		return resolvedWorkerBuildInputs{}, err
	}
	internalTree, err := gitTreeEntry(ctx, client, repository, commit.Commit.Tree.SHA, "internal", "tree")
	if err != nil {
		return resolvedWorkerBuildInputs{}, err
	}
	deliverySourceDigestPackageTree, err := gitTreeEntry(ctx, client, repository, internalTree, "deliverysource", "tree")
	if err != nil {
		return resolvedWorkerBuildInputs{}, err
	}
	goModBlob, err := gitTreeEntry(ctx, client, repository, commit.Commit.Tree.SHA, "go.mod", "blob")
	if err != nil {
		return resolvedWorkerBuildInputs{}, err
	}
	goSumBlob, err := gitTreeEntry(ctx, client, repository, commit.Commit.Tree.SHA, "go.sum", "blob")
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
	return resolvedWorkerBuildInputs{CommitSHA: commit.SHA, Config: config, Identity: workerBuildInputIdentity(config, workerTree, deliverySourceDigestCommandTree, deliverySourceDigestPackageTree, goModBlob, goSumBlob, publisherWorkflow)}, nil
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

func workerBuildInputIdentity(config Config, workerTree, deliverySourceDigestCommandTree, deliverySourceDigestPackageTree, goModBlob, goSumBlob, publisherWorkflow string) string {
	inputs := workerBuildInputs{
		SchemaVersion:                   6,
		DeployWorkerTree:                workerTree,
		DeliverySourceDigestCommandTree: deliverySourceDigestCommandTree,
		DeliverySourceDigestPackageTree: deliverySourceDigestPackageTree,
		GoModBlob:                       goModBlob,
		GoSumBlob:                       goSumBlob,
		PublishWorkerWorkflowBlob:       publisherWorkflow,
		Codex:                           config.Codex,
		GitHubCLI:                       config.GitHubCLI,
		Go:                              config.Go,
		NoMistakes:                      config.NoMistakes,
		Worker:                          config.Worker,
	}
	encoded, _ := json.Marshal(canonicalizeWorkerBuildInputs(inputs))
	digest := sha256.Sum256(encoded)
	return fmt.Sprintf("%x", digest)
}

func canonicalizeWorkerBuildInputs(inputs workerBuildInputs) canonicalWorkerBuildInputs {
	return canonicalWorkerBuildInputs{
		SchemaVersion:                   inputs.SchemaVersion,
		DeployWorkerTree:                base64.StdEncoding.EncodeToString([]byte(inputs.DeployWorkerTree)),
		DeliverySourceDigestCommandTree: base64.StdEncoding.EncodeToString([]byte(inputs.DeliverySourceDigestCommandTree)),
		DeliverySourceDigestPackageTree: base64.StdEncoding.EncodeToString([]byte(inputs.DeliverySourceDigestPackageTree)),
		GoModBlob:                       base64.StdEncoding.EncodeToString([]byte(inputs.GoModBlob)),
		GoSumBlob:                       base64.StdEncoding.EncodeToString([]byte(inputs.GoSumBlob)),
		PublishWorkerWorkflowBlob:       base64.StdEncoding.EncodeToString([]byte(inputs.PublishWorkerWorkflowBlob)),
		Codex: ToolPin{
			Version: base64.StdEncoding.EncodeToString([]byte(inputs.Codex.Version)),
		},
		GitHubCLI: GitHubCLIPin{
			Version:          base64.StdEncoding.EncodeToString([]byte(inputs.GitHubCLI.Version)),
			LinuxAMD64SHA256: base64.StdEncoding.EncodeToString([]byte(inputs.GitHubCLI.LinuxAMD64SHA256)),
		},
		Go: GoPin{
			Version:          base64.StdEncoding.EncodeToString([]byte(inputs.Go.Version)),
			LinuxAMD64SHA256: base64.StdEncoding.EncodeToString([]byte(inputs.Go.LinuxAMD64SHA256)),
		},
		NoMistakes: NoMistakesPin{
			Version:            base64.StdEncoding.EncodeToString([]byte(inputs.NoMistakes.Version)),
			UpstreamRepository: base64.StdEncoding.EncodeToString([]byte(inputs.NoMistakes.UpstreamRepository)),
			UpstreamCommit:     base64.StdEncoding.EncodeToString([]byte(inputs.NoMistakes.UpstreamCommit)),
			ForkRepository:     base64.StdEncoding.EncodeToString([]byte(inputs.NoMistakes.ForkRepository)),
			ForkCommit:         base64.StdEncoding.EncodeToString([]byte(inputs.NoMistakes.ForkCommit)),
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
