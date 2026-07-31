package doctor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	Worker WorkerPin
}

func (DockerCheck) Name() string { return "Docker Worker contract" }

func (c DockerCheck) Run(ctx context.Context) Result {
	executor := OSExecutor{}
	info, err := executor.Run(ctx, []string{"docker", "info", "--format", "{{.OSType}}/{{.Architecture}}"})
	if err != nil || strings.TrimSpace(string(info)) != "linux/x86_64" {
		return Result{Status: Fail, Summary: fmt.Sprintf("Docker Engine must be linux/x86_64: %v (%s)", err, strings.TrimSpace(string(info)))}
	}
	localImageID := c.Worker.LocalBuildID
	imageID, err := executor.Run(ctx, []string{"docker", "image", "inspect", localImageID, "--format", "{{.Id}}"})
	if err != nil || strings.TrimSpace(string(imageID)) != localImageID {
		return Result{Status: Fail, Summary: fmt.Sprintf("local Worker image does not match pin: %v (%s)", err, strings.TrimSpace(string(imageID)))}
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
		localImageID, "sh", "-ceu", script,
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
	required := []string{"gateway=ok", "mount=ok", "0.146.0", "v1.41.2"}
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
	Pin GitHubCredentialPin
}

func (GitHubCredentialCheck) Name() string { return "GitHub credential scope" }

func (c GitHubCredentialCheck) Run(ctx context.Context) Result {
	if strings.TrimSpace(c.Pin.ApprovedBy) == "" || strings.TrimSpace(c.Pin.ApprovedAt) == "" ||
		!sha256Pattern.MatchString(c.Pin.FingerprintSHA256) {
		return Result{Status: Fail, Summary: "least-privilege credential has not been human-attested"}
	}
	executor := OSExecutor{}
	token, err := executor.Run(ctx, []string{"gh", "auth", "token"})
	if err != nil {
		return Result{Status: Fail, Summary: "cannot read active credential for fingerprint verification"}
	}
	if credentialFingerprint(string(token)) != c.Pin.FingerprintSHA256 {
		return Result{Status: Fail, Summary: "active credential does not match the human-attested fingerprint"}
	}
	if c.Pin.Kind == "fine-grained-pat" {
		if !strings.HasPrefix(strings.TrimSpace(string(token)), "github_pat_") {
			return Result{Status: Fail, Summary: "active credential is not the attested fine-grained PAT"}
		}
	}
	output, err := executor.Run(ctx, []string{"gh", "api", "--paginate", "user/repos?per_page=100&affiliation=owner,collaborator,organization_member"})
	if err != nil {
		return Result{Status: Fail, Summary: fmt.Sprintf("enumerate credential repository access: %v", err)}
	}
	var repositories []struct {
		FullName string `json:"full_name"`
		Private  bool   `json:"private"`
	}
	if err := json.Unmarshal(output, &repositories); err != nil {
		return Result{Status: Fail, Summary: fmt.Sprintf("decode credential repository access: %v", err)}
	}
	allowed := make(map[string]bool, len(c.Pin.AllowedRepositories))
	for _, repository := range c.Pin.AllowedRepositories {
		allowed[repository] = true
	}
	for _, repository := range repositories {
		if repository.Private && !allowed[repository.FullName] {
			return Result{Status: Fail, Summary: "credential can access a private repository outside its declared allowlist"}
		}
	}
	return Result{Status: Pass, Summary: fmt.Sprintf("%s restricted to %d declared repositories; permissions human-attested", c.Pin.Kind, len(allowed))}
}

type GitHubCheck struct {
	GitHub     GitHubPin
	NoMistakes NoMistakesPin
	Executor   Executor
}

func (GitHubCheck) Name() string { return "GitHub protected integration contract" }

func (c GitHubCheck) Run(ctx context.Context) Result {
	executor := c.Executor
	if executor == nil {
		executor = OSExecutor{}
	}
	repository, err := executor.Run(ctx, []string{"gh", "api", "repos/" + c.GitHub.TestRepository})
	if err != nil {
		return Result{Status: Fail, Summary: fmt.Sprintf("read private test repository: %v (%s)", err, strings.TrimSpace(string(repository)))}
	}
	var repo struct {
		Private       bool   `json:"private"`
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.Unmarshal(repository, &repo); err != nil {
		return Result{Status: Fail, Summary: err.Error()}
	}
	if !repo.Private || repo.DefaultBranch != c.GitHub.DefaultBranch {
		return Result{Status: Fail, Summary: fmt.Sprintf("repository private=%t default_branch=%s", repo.Private, repo.DefaultBranch)}
	}
	branch, err := executor.Run(ctx, []string{"gh", "api", "repos/" + c.GitHub.TestRepository + "/branches/" + c.GitHub.DefaultBranch})
	if err != nil {
		return Result{Status: Fail, Summary: fmt.Sprintf("read default branch head: %v (%s)", err, strings.TrimSpace(string(branch)))}
	}
	var branchResponse struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if json.Unmarshal(branch, &branchResponse) != nil || branchResponse.Commit.SHA == "" {
		return Result{Status: Fail, Summary: "default branch did not report a head SHA"}
	}
	runs, err := executor.Run(ctx, []string{"gh", "run", "list", "-R", c.GitHub.TestRepository, "--workflow", "workflow-contract", "--branch", c.GitHub.DefaultBranch, "--limit", "20", "--json", "status,conclusion,headSha"})
	if err != nil {
		return Result{Status: Fail, Summary: fmt.Sprintf("list required workflow runs: %v (%s)", err, strings.TrimSpace(string(runs)))}
	}
	var workflowRuns []struct {
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		HeadSHA    string `json:"headSha"`
	}
	if json.Unmarshal(runs, &workflowRuns) != nil {
		return Result{Status: Fail, Summary: "required workflow runs were not valid JSON"}
	}
	contractPassed := false
	for _, run := range workflowRuns {
		if run.HeadSHA == branchResponse.Commit.SHA && run.Status == "completed" && run.Conclusion == "success" {
			contractPassed = true
			break
		}
	}
	if !contractPassed {
		return Result{Status: Fail, Summary: "required workflow has not succeeded for the current default-branch revision"}
	}
	protection, err := executor.Run(ctx, []string{"gh", "api", "repos/" + c.GitHub.TestRepository + "/branches/" + c.GitHub.DefaultBranch + "/protection"})
	if err != nil {
		return Result{Status: Fail, Summary: fmt.Sprintf("branch protection unavailable: %v (%s)", err, strings.TrimSpace(string(protection)))}
	}
	var rules struct {
		RequiredStatusChecks struct {
			Contexts []string `json:"contexts"`
		} `json:"required_status_checks"`
		RequiredPullRequestReviews struct {
			RequiredApprovingReviewCount int `json:"required_approving_review_count"`
		} `json:"required_pull_request_reviews"`
	}
	if err := json.Unmarshal(protection, &rules); err != nil {
		return Result{Status: Fail, Summary: err.Error()}
	}
	if !contains(rules.RequiredStatusChecks.Contexts, c.GitHub.RequiredCheck) ||
		rules.RequiredPullRequestReviews.RequiredApprovingReviewCount < c.GitHub.RequiredReviewCount {
		return Result{Status: Fail, Summary: "branch protection does not require the pinned check and human approval"}
	}
	release, err := executor.Run(ctx, []string{"gh", "api", "repos/" + c.NoMistakes.ForkRepository + "/releases/tags/" + c.NoMistakes.ForkRelease})
	if err != nil {
		return Result{Status: Fail, Summary: fmt.Sprintf("fork release is not pinned to upstream commit: %v", err)}
	}
	var forkRelease struct {
		TargetCommitish string `json:"target_commitish"`
	}
	if json.Unmarshal(release, &forkRelease) != nil || forkRelease.TargetCommitish != c.NoMistakes.UpstreamCommit {
		return Result{Status: Fail, Summary: "fork release target does not equal the pinned upstream commit"}
	}
	return Result{Status: Pass, Summary: "private test repository, successful required check, human-review protection, and pinned fork release verified"}
}

func randomToken() (string, error) {
	value := make([]byte, 24)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
