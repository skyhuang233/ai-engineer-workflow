package doctor

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/skyhuang233/workflow/internal/credential"
	githubapi "github.com/skyhuang233/workflow/internal/github"
	"github.com/skyhuang233/workflow/internal/store"
	"github.com/skyhuang233/workflow/internal/worker"
)

type SQLiteCheck struct {
	Path string
}

func (SQLiteCheck) Name() string { return "SQLite durability and locking" }

func (c SQLiteCheck) Run(ctx context.Context) Result {
	database, err := store.Open(ctx, c.Path)
	if err != nil {
		return Result{Status: Fail, Summary: err.Error()}
	}
	defer database.Close()
	health, err := database.Health(ctx)
	if err != nil {
		return Result{Status: Fail, Summary: err.Error()}
	}
	if !strings.EqualFold(health.JournalMode, "wal") || health.Synchronous != 2 ||
		!health.ForeignKeys || health.Integrity != "ok" || !health.WriteLocking {
		return Result{Status: Fail, Summary: fmt.Sprintf("journal=%s synchronous=%d foreign_keys=%t integrity=%s write_locking=%t",
			health.JournalMode, health.Synchronous, health.ForeignKeys, health.Integrity, health.WriteLocking)}
	}
	return Result{Status: Pass, Summary: "WAL, synchronous=FULL, foreign keys, integrity, and serialized writer locking verified"}
}

type DockerCheck struct {
	Manifest WorkerReleaseManifest
}

func (DockerCheck) Name() string { return "Docker Worker contract" }

func (c DockerCheck) Run(ctx context.Context) Result {
	executor := OSExecutor{}
	info, err := executor.Run(ctx, []string{"docker", "info", "--format", "{{.OSType}}/{{.Architecture}}"})
	if err != nil || strings.TrimSpace(string(info)) != "linux/x86_64" {
		return Result{Status: Fail, Summary: fmt.Sprintf("Docker Engine must be linux/x86_64: %v (%s)", err, strings.TrimSpace(string(info)))}
	}
	image := c.Manifest.Image
	if output, err := executor.Run(ctx, []string{"docker", "pull", image}); err != nil {
		return Result{Status: Fail, Summary: fmt.Sprintf("pull exact Worker digest: %v (%s)", err, strings.TrimSpace(string(output)))}
	}

	root, err := os.MkdirTemp("", "workflow-doctor-*")
	if err != nil {
		return Result{Status: Fail, Summary: err.Error()}
	}
	defer os.RemoveAll(root)
	workspace := filepath.Join(root, "workspace")
	codexState := filepath.Join(root, "codex")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		return Result{Status: Fail, Summary: err.Error()}
	}
	if err := os.MkdirAll(codexState, 0o700); err != nil {
		return Result{Status: Fail, Summary: err.Error()}
	}
	if err := os.WriteFile(filepath.Join(workspace, "host-marker"), []byte("doctor"), 0o600); err != nil {
		return Result{Status: Fail, Summary: err.Error()}
	}

	token, err := randomToken()
	if err != nil {
		return Result{Status: Fail, Summary: err.Error()}
	}
	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		return Result{Status: Fail, Summary: fmt.Sprintf("listen for Gateway probe: %v", err)}
	}
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte("gateway=ok"))
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Shutdown(context.Background())
	port := listener.Addr().(*net.TCPAddr).Port

	script := `test "$(cat /workspace/host-marker)" = doctor
printf worker > /workspace/container-marker
curl --fail --silent --show-error -H "Authorization: Bearer ${WORKFLOW_GATEWAY_PROBE_TOKEN}" "http://host.docker.internal:${WORKFLOW_GATEWAY_PROBE_PORT}/health"
printf "\nmount=ok\n"
codex --version
no-mistakes --version
env | cut -d= -f1`
	output, err := executor.Run(ctx, []string{
		"docker", "run", "--rm",
		"--add-host", "host.docker.internal:host-gateway",
		"--mount", "type=bind,src=" + workspace + ",dst=/workspace",
		"--mount", "type=bind,src=" + codexState + ",dst=/codex-state",
		"--env", "CODEX_HOME=/codex-state",
		"--env", "WORKFLOW_GATEWAY_PROBE_TOKEN=" + token,
		"--env", fmt.Sprintf("WORKFLOW_GATEWAY_PROBE_PORT=%d", port),
		image, "sh", "-ceu", script,
	})
	text := string(output)
	if err != nil {
		return Result{Status: Fail, Summary: fmt.Sprintf("Worker probe: %v (%s)", err, strings.TrimSpace(text))}
	}
	marker, markerErr := os.ReadFile(filepath.Join(workspace, "container-marker"))
	if markerErr != nil || string(marker) != "worker" {
		return Result{Status: Fail, Summary: "container-to-host workspace write was not preserved"}
	}
	for _, line := range strings.Split(text, "\n") {
		if worker.IsGitHubCredentialName(strings.TrimSpace(line)) {
			return Result{Status: Fail, Summary: "Worker environment contains a forbidden GitHub write credential name"}
		}
	}
	required := []string{"gateway=ok", "mount=ok", c.Manifest.CodexVersion, c.Manifest.NoMistakesVersion}
	for _, value := range required {
		if !strings.Contains(text, value) {
			return Result{Status: Fail, Summary: fmt.Sprintf("Worker probe omitted required evidence %q", value)}
		}
	}
	return Result{Status: Pass, Summary: "Linux Engine, bind mounts, host.docker.internal Gateway, pinned tools, and absence of GitHub write credentials verified"}
}

type WorkerRegistryCheck struct {
	Image string
}

func (WorkerRegistryCheck) Name() string { return "Published Worker digest" }

func (c WorkerRegistryCheck) Run(ctx context.Context) Result {
	output, err := (OSExecutor{}).Run(ctx, []string{"docker", "manifest", "inspect", c.Image})
	if err != nil {
		return Result{Status: Fail, Summary: fmt.Sprintf("registry cannot resolve pinned Worker digest: %v (%s)", err, strings.TrimSpace(string(output)))}
	}
	return Result{Status: Pass, Summary: "registry resolves the exact pinned Worker manifest digest"}
}

type GitHubCredentialCheck struct {
	Pin                   GitHubCredentialPin
	IntegrationRepository string
	Credentials           credential.Store
	Verification          store.GatewayCredentialVerification
	APIBase               string
}

func (GitHubCredentialCheck) Name() string { return "Gateway Credential contract" }

func (c GitHubCredentialCheck) Run(ctx context.Context) Result {
	if c.Credentials == nil || !sha256Pattern.MatchString(c.Verification.FingerprintSHA256) {
		return Result{Status: Fail, Summary: "Gateway Credential has not completed its live write contract"}
	}
	token, err := c.Credentials.Get(ctx, credential.GatewayTarget)
	if err != nil {
		return Result{Status: Fail, Summary: "Gateway Credential is unavailable in Windows Credential Manager"}
	}
	if credential.Fingerprint(token) != c.Verification.FingerprintSHA256 {
		return Result{Status: Fail, Summary: "Gateway Credential differs from the live-contract verification"}
	}
	if !strings.HasPrefix(strings.TrimSpace(token), "github_pat_") {
		return Result{Status: Fail, Summary: "Gateway Credential is not a fine-grained PAT"}
	}
	var identity struct {
		Login string `json:"login"`
	}
	err = githubGET(ctx, c.APIBase, token, "user", &identity)
	if err != nil {
		return Result{Status: Fail, Summary: fmt.Sprintf("read Gateway identity: %v", err)}
	}
	if identity.Login != c.Pin.Owner || c.Verification.Owner != c.Pin.Owner {
		return Result{Status: Fail, Summary: "Gateway Credential owner does not match the verified owner"}
	}
	if c.Verification.IntegrationRepository != c.IntegrationRepository {
		return Result{Status: Fail, Summary: "Gateway Credential verification does not match the configured integration repository"}
	}
	return Result{Status: Pass, Summary: "Credential Manager secret matches the verified fine-grained PAT, owner, and integration repository"}
}

type GitHubCheck struct {
	GitHub      GitHubPin
	NoMistakes  NoMistakesPin
	Credentials credential.Store
	APIBase     string
}

func (GitHubCheck) Name() string { return "GitHub Owner-Guarded integration contract" }

func (c GitHubCheck) Run(ctx context.Context) Result {
	if c.Credentials == nil {
		return Result{Status: Fail, Summary: "Gateway Credential store is unavailable"}
	}
	token, err := c.Credentials.Get(ctx, credential.GatewayTarget)
	if err != nil {
		return Result{Status: Fail, Summary: "Gateway Credential is unavailable"}
	}
	var repo struct {
		Private       bool   `json:"private"`
		DefaultBranch string `json:"default_branch"`
	}
	if err := githubGET(ctx, c.APIBase, token, "repos/"+c.GitHub.TestRepository, &repo); err != nil {
		return Result{Status: Fail, Summary: fmt.Sprintf("read integration repository: %v", err)}
	}
	if repo.Private || repo.DefaultBranch != c.GitHub.DefaultBranch {
		return Result{Status: Fail, Summary: fmt.Sprintf("integration repository private=%t default_branch=%s", repo.Private, repo.DefaultBranch)}
	}
	var branch struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	branchPath := "repos/" + c.GitHub.TestRepository + "/git/ref/heads/" + url.PathEscape(c.GitHub.DefaultBranch)
	if err := githubGET(ctx, c.APIBase, token, branchPath, &branch); err != nil || branch.Object.SHA == "" {
		return Result{Status: Fail, Summary: fmt.Sprintf("read default branch head: %v", err)}
	}
	var workflows struct {
		Workflows []struct {
			ID   int64  `json:"id"`
			Path string `json:"path"`
		} `json:"workflows"`
	}
	if err := githubGET(ctx, c.APIBase, token, "repos/"+c.GitHub.TestRepository+"/actions/workflows", &workflows); err != nil {
		return Result{Status: Fail, Summary: fmt.Sprintf("read integration workflows: %v", err)}
	}
	var workflowID int64
	for _, workflow := range workflows.Workflows {
		if workflow.Path == c.GitHub.WorkflowPath {
			workflowID = workflow.ID
			break
		}
	}
	if workflowID == 0 {
		return Result{Status: Fail, Summary: "integration workflow at the configured path is missing"}
	}
	var runs struct {
		WorkflowRuns []struct {
			HeadSHA    string `json:"head_sha"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		} `json:"workflow_runs"`
	}
	runsPath := fmt.Sprintf("repos/%s/actions/workflows/%d/runs?branch=%s&per_page=20", c.GitHub.TestRepository, workflowID, url.QueryEscape(c.GitHub.DefaultBranch))
	if err := githubGET(ctx, c.APIBase, token, runsPath, &runs); err != nil {
		return Result{Status: Fail, Summary: fmt.Sprintf("read integration workflow runs: %v", err)}
	}
	contractPassed := false
	for _, run := range runs.WorkflowRuns {
		if run.HeadSHA == branch.Object.SHA && run.Status == "completed" && run.Conclusion == "success" {
			contractPassed = true
			break
		}
	}
	if !contractPassed {
		return Result{Status: Fail, Summary: "integration workflow has not succeeded for the current default-branch revision"}
	}
	if result := requirePublicProvenanceRepository(ctx, c.APIBase, token, c.NoMistakes.UpstreamRepository, "no-mistakes upstream"); result != nil {
		return *result
	}
	if result := requirePublicProvenanceRepository(ctx, c.APIBase, token, c.NoMistakes.ForkRepository, "no-mistakes fork"); result != nil {
		return *result
	}
	var upstreamCommit struct {
		SHA string `json:"sha"`
	}
	if err := githubGET(ctx, c.APIBase, token, "repos/"+c.NoMistakes.UpstreamRepository+"/git/commits/"+c.NoMistakes.UpstreamCommit, &upstreamCommit); err != nil || upstreamCommit.SHA != c.NoMistakes.UpstreamCommit {
		return Result{Status: Fail, Summary: "pinned upstream commit is unavailable from the configured no-mistakes upstream repository"}
	}
	var forkRelease struct {
		TargetCommitish string `json:"target_commitish"`
		Assets          []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"assets"`
	}
	if err := githubGET(ctx, c.APIBase, token, "repos/"+c.NoMistakes.ForkRepository+"/releases/tags/"+c.NoMistakes.ForkRelease, &forkRelease); err != nil ||
		forkRelease.TargetCommitish != c.NoMistakes.UpstreamCommit {
		return Result{Status: Fail, Summary: "fork release target does not equal the pinned upstream commit"}
	}
	assetName := "no-mistakes-" + c.NoMistakes.Version + "-linux-amd64.tar.gz"
	assetID := int64(0)
	for _, asset := range forkRelease.Assets {
		if asset.Name != assetName {
			continue
		}
		if assetID != 0 {
			return Result{Status: Fail, Summary: "fork release has multiple pinned no-mistakes Linux assets"}
		}
		assetID = asset.ID
	}
	if assetID == 0 {
		return Result{Status: Fail, Summary: "fork release is missing the pinned no-mistakes Linux asset"}
	}
	asset, err := githubapi.NewClient(c.APIBase, token, nil).RequestBytes(ctx,
		fmt.Sprintf("/repos/%s/releases/assets/%d", c.NoMistakes.ForkRepository, assetID), "application/octet-stream")
	if err != nil {
		return Result{Status: Fail, Summary: fmt.Sprintf("download pinned no-mistakes Linux asset: %v", err)}
	}
	digest := sha256.Sum256(asset)
	if hex.EncodeToString(digest[:]) != c.NoMistakes.LinuxAMD64SHA256 {
		return Result{Status: Fail, Summary: "pinned no-mistakes Linux asset checksum does not match"}
	}
	return Result{Status: Pass, Summary: "integration workflow, pinned fork release, and Linux asset checksum verified; owner-only merge remains the governance boundary"}
}

func requirePublicProvenanceRepository(ctx context.Context, apiBase, token, repository, name string) *Result {
	var target struct {
		Private bool `json:"private"`
	}
	if err := githubGET(ctx, apiBase, token, "repos/"+repository, &target); err != nil {
		return &Result{Status: Fail, Summary: fmt.Sprintf("read %s repository: %v", name, err)}
	}
	if target.Private {
		return &Result{Status: Fail, Summary: name + " repository must be public"}
	}
	return nil
}

func githubGET(ctx context.Context, apiBase, token, path string, destination any) error {
	return githubapi.NewClient(apiBase, token, nil).RequestJSON(ctx, http.MethodGet, "/"+path, nil, destination)
}

func randomToken() (string, error) {
	value := make([]byte, 24)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}
