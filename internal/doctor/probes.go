package doctor

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	candidateoutput "github.com/skyhuang233/workflow/internal/candidate"
	"github.com/skyhuang233/workflow/internal/codexauth"
	"github.com/skyhuang233/workflow/internal/credential"
	githubapi "github.com/skyhuang233/workflow/internal/github"
	"github.com/skyhuang233/workflow/internal/githubcredential"
	"github.com/skyhuang233/workflow/internal/store"
	"github.com/skyhuang233/workflow/internal/worker"
)

type SQLiteCheck struct {
	Path       string
	BackupPath string
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
	backupPath := c.BackupPath
	if backupPath == "" {
		backupPath = c.Path + ".backup"
	}
	metrics, metricsErr := database.OperationalMetrics(ctx, backupPath, time.Now().UTC())
	if metricsErr != nil {
		if !os.IsNotExist(rootCause(metricsErr)) {
			return Result{Status: Fail, Summary: fmt.Sprintf("read SQLite backup operations metrics: %v", metricsErr), Err: metricsErr}
		}
		return Result{Status: Pass, Summary: "WAL, synchronous=FULL, foreign keys, integrity, and serialized writer locking verified; backup=unavailable"}
	}
	return Result{Status: Pass, Summary: fmt.Sprintf("WAL, synchronous=FULL, foreign keys, integrity, and serialized writer locking verified; backup_age=%s drill_succeeded=%t outbox_age=%s reconcile_lag=%s", metrics.BackupAge.Round(time.Second), metrics.DrillSucceeded, metrics.OutboxAge.Round(time.Second), metrics.ReconcileLag.Round(time.Second))}
}

func rootCause(err error) error {
	for next := errors.Unwrap(err); next != nil; next = errors.Unwrap(err) {
		err = next
	}
	return err
}

type DockerCheck struct {
	Manifest WorkerReleaseManifest
	Executor Executor
}

func verifyWorkerNoMistakesBuildMetadata(output, expectedCommit string) error {
	var revision, modified string
	var revisionCount, modifiedCount int
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != "build" {
			continue
		}
		key, value, ok := strings.Cut(fields[1], "=")
		if !ok {
			continue
		}
		switch key {
		case "vcs.revision":
			revision, revisionCount = value, revisionCount+1
		case "vcs.modified":
			modified, modifiedCount = value, modifiedCount+1
		}
	}
	if revisionCount != 1 || revision != expectedCommit {
		return fmt.Errorf("Worker no-mistakes VCS revision %q does not equal pinned fork commit %q", revision, expectedCommit)
	}
	if modifiedCount != 1 || modified != "false" {
		return fmt.Errorf("Worker no-mistakes VCS modified state %q is not a clean build", modified)
	}
	return nil
}

type WorkerCodexSessionCheck struct {
	Executor  Executor
	Image     string
	AuthFile  string
	Nonce     string
	RemoveAll func(string) error
}

func (WorkerCodexSessionCheck) Name() string { return "Worker Codex authentication and session resume" }

func (c WorkerCodexSessionCheck) Run(ctx context.Context) (result Result) {
	if c.Executor == nil || strings.TrimSpace(c.Image) == "" || strings.TrimSpace(c.AuthFile) == "" {
		return Result{Status: Fail, Summary: "Worker Codex authentication check is incomplete"}
	}
	root, err := os.MkdirTemp("", "workflow-doctor-codex-*")
	if err != nil {
		return Result{Status: Fail, Summary: err.Error()}
	}
	removeAll := c.RemoveAll
	if removeAll == nil {
		removeAll = os.RemoveAll
	}
	defer func() {
		if cleanupErr := removeAll(root); cleanupErr != nil {
			primaryErr := result.Err
			if primaryErr == nil && result.Status == Fail && result.Summary != "" {
				primaryErr = errors.New(result.Summary)
			}
			cleanupSummary := fmt.Sprintf("remove temporary Worker Codex authentication directory %q: %v", root, cleanupErr)
			if result.Summary == "" {
				result.Summary = cleanupSummary
			} else {
				result.Summary += "; " + cleanupSummary
			}
			result.Status = Fail
			result.Err = errors.Join(primaryErr, cleanupErr)
		}
	}()
	workspace := filepath.Join(root, "workspace")
	codexState := filepath.Join(root, "codex-state")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		return Result{Status: Fail, Summary: err.Error()}
	}
	if err := os.MkdirAll(codexState, 0o700); err != nil {
		return Result{Status: Fail, Summary: err.Error()}
	}
	if err := codexauth.Seed(c.AuthFile, codexState); err != nil {
		return Result{Status: Fail, Summary: err.Error()}
	}
	schemaPath := filepath.Join(codexState, "output-schema.json")
	if err := os.WriteFile(schemaPath, []byte(candidateoutput.Schema), 0o600); err != nil {
		return Result{Status: Fail, Summary: err.Error()}
	}
	nonce := c.Nonce
	if nonce == "" {
		nonce, err = randomToken()
		if err != nil {
			return Result{Status: Fail, Summary: err.Error()}
		}
	}
	initial, err := c.Executor.Run(ctx, workerCodexCommand(c.Image, workspace, codexState,
		"exec", "--sandbox", "read-only", "--json", "--output-schema", "/codex-state/output-schema.json", "--skip-git-repo-check",
		"Remember this nonce for the next turn: "+nonce+`. Return the required JSON with summary "phase-one", commit null, one passed check named "doctor schema probe", and plan_amendment null.`))
	if err != nil {
		return workerCodexFailure("create authenticated Worker Codex session", initial, err)
	}
	if _, err := candidateoutput.ExtractCodexCandidate(initial); err != nil {
		return Result{Status: Fail, Summary: "Worker Codex create schema probe returned an invalid structured response"}
	}
	sessionID := jsonEventString(initial, "thread.started", "thread_id")
	if sessionID == "" {
		return Result{Status: Fail, Summary: "Worker Codex did not emit a persistent session ID"}
	}
	resumed, err := c.Executor.Run(ctx, workerCodexCommand(c.Image, workspace, codexState,
		"exec", "--sandbox", "read-only", "resume", "--json", "--output-schema", "/codex-state/output-schema.json", "--skip-git-repo-check", sessionID,
		`Return the required JSON with the nonce from the previous turn as summary, commit null, one passed check named "doctor schema probe", and plan_amendment null.`))
	if err != nil {
		return workerCodexFailure("resume authenticated Worker Codex session", resumed, err)
	}
	structured, err := candidateoutput.ExtractCodexCandidate(resumed)
	if err != nil {
		return Result{Status: Fail, Summary: "Worker Codex resume schema probe returned an invalid structured response"}
	}
	var candidate struct {
		Summary string `json:"summary"`
	}
	if json.Unmarshal(structured, &candidate) != nil || candidate.Summary != nonce {
		return Result{Status: Fail, Summary: "resumed Worker Codex session did not recall prior-turn context"}
	}
	return Result{Status: Pass, Summary: "pinned Worker accepted the Candidate schema, authenticated, and resumed persisted context"}
}

func workerCodexCommand(image, workspace, codexState string, command ...string) []string {
	args := []string{"docker", "run", "--rm"}
	args = append(args, worker.CodexSandboxDockerArgs()...)
	args = append(args,
		"--workdir", "/workspace",
		"--mount", "type=bind,source="+workspace+",target=/workspace",
		"--mount", "type=bind,source="+codexState+",target=/codex-state",
		"--env", "CODEX_HOME=/codex-state", image, "codex",
	)
	return append(args, command...)
}

func workerCodexFailure(action string, output []byte, err error) Result {
	lower := strings.ToLower(string(output))
	if strings.Contains(lower, "401 unauthorized") || strings.Contains(lower, "missing bearer or basic authentication") {
		return Result{Status: Fail, Summary: "Worker Codex authentication was rejected"}
	}
	return Result{Status: Fail, Summary: fmt.Sprintf("%s: %v", action, err)}
}

func (DockerCheck) Name() string { return "Docker Worker contract" }

func (c DockerCheck) Run(ctx context.Context) Result {
	var executor Executor = c.Executor
	if executor == nil {
		executor = OSExecutor{}
	}
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
gh --version
go version
no-mistakes --version
command -v delivery-source-digest
no-mistakes daemon start
daemon_status="$(no-mistakes daemon status)"
printf "%s\n" "$daemon_status"
case "$daemon_status" in
  *"daemon running"*) ;;
  *) echo "no-mistakes daemon is not running" >&2; exit 1 ;;
esac
env | cut -d= -f1`
	dockerCommand := []string{"docker", "run", "--rm"}
	dockerCommand = append(dockerCommand, worker.CodexSandboxDockerArgs()...)
	dockerCommand = append(dockerCommand,
		"--add-host", "host.docker.internal:host-gateway",
		"--mount", "type=bind,src="+workspace+",dst=/workspace",
		"--mount", "type=bind,src="+codexState+",dst=/codex-state",
		"--env", "CODEX_HOME=/codex-state",
		"--env", "NM_HOME=/codex-state/no-mistakes",
		"--env", "WORKFLOW_GATEWAY_PROBE_TOKEN="+token,
		"--env", fmt.Sprintf("WORKFLOW_GATEWAY_PROBE_PORT=%d", port),
		image, "sh", "-ceu", script,
	)
	output, err := executor.Run(ctx, dockerCommand)
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
	required := []string{"gateway=ok", "mount=ok", c.Manifest.CodexVersion, "gh version " + c.Manifest.GitHubCLIVersion, "go" + c.Manifest.GoVersion, c.Manifest.NoMistakesVersion, "daemon running"}
	for _, value := range required {
		if !strings.Contains(text, value) {
			return Result{Status: Fail, Summary: fmt.Sprintf("Worker probe omitted required evidence %q", value)}
		}
	}
	metadataOutput, err := executor.Run(ctx, []string{
		"docker", "run", "--rm",
		"--entrypoint", "/usr/local/go/bin/go",
		image, "version", "-m", "/usr/local/bin/no-mistakes",
	})
	if err != nil {
		return Result{Status: Fail, Summary: fmt.Sprintf("read Worker no-mistakes build metadata: %v (%s)", err, strings.TrimSpace(string(metadataOutput)))}
	}
	if err := verifyWorkerNoMistakesBuildMetadata(string(metadataOutput), c.Manifest.NoMistakesForkCommit); err != nil {
		return Result{Status: Fail, Summary: err.Error()}
	}
	return Result{Status: Pass, Summary: "Linux Engine, bind mounts, host.docker.internal Gateway, pinned tools, no-mistakes daemon, and absence of GitHub write credentials verified"}
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

type GitHubPATCheck struct {
	Pin          GitHubCredentialPin
	Token        string
	Verification store.GitHubPATVerification
	APIBase      string
	Client       *http.Client
}

func (GitHubPATCheck) Name() string { return "Control Plane classic GitHub PAT contract" }
func (c GitHubPATCheck) Run(ctx context.Context) Result {
	if c.Token == "" || c.Pin.Kind != "classic-pat" || !strings.EqualFold(c.Pin.Owner, c.Verification.Owner) || credential.Fingerprint(c.Token) != c.Verification.FingerprintSHA256 || c.Verification.Status != "verified" {
		return Result{Status: Fail, Summary: "classic PAT does not match its verified owner-bound record"}
	}
	live, err := (githubcredential.Verifier{APIBase: c.APIBase, Client: c.Client}).Verify(ctx, c.Token, c.Pin.Owner)
	if err != nil {
		return Result{Status: Fail, Summary: fmt.Sprintf("verify classic PAT live contract: %v", err), Err: err}
	}
	if live.FingerprintSHA256 != c.Verification.FingerprintSHA256 || !strings.EqualFold(live.Login, c.Verification.Login) {
		return Result{Status: Fail, Summary: "classic PAT live identity differs from persisted verification"}
	}
	return Result{Status: Pass, Summary: "classic PAT identity, repo/workflow scopes, owner binding, and fingerprint verified"}
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
		return Result{Status: Fail, Summary: "Control Plane GitHub credential source is unavailable"}
	}
	token, err := c.Credentials.Get(ctx, credential.GatewayTarget)
	if err != nil {
		return Result{Status: Fail, Summary: "Control Plane GitHub credential is unavailable"}
	}
	var repo githubapi.RepositoryMetadata
	if err := githubGET(ctx, c.APIBase, token, "repos/"+c.GitHub.TestRepository, &repo); err != nil {
		return Result{Status: Fail, Summary: fmt.Sprintf("read integration repository: %v", err), Err: err}
	}
	if err := repo.ValidateOwnerGuarded(c.GitHub.TestRepository, c.GitHub.Credential.Owner); err != nil {
		return Result{Status: Fail, Summary: fmt.Sprintf("integration repository admission: %v", err), Err: err}
	}
	if repo.DefaultBranch != c.GitHub.DefaultBranch {
		return Result{Status: Fail, Summary: fmt.Sprintf("integration repository default_branch=%s", repo.DefaultBranch)}
	}
	var branch struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	branchPath := "repos/" + c.GitHub.TestRepository + "/git/ref/heads/" + url.PathEscape(c.GitHub.DefaultBranch)
	if err := githubGET(ctx, c.APIBase, token, branchPath, &branch); err != nil || branch.Object.SHA == "" {
		return Result{Status: Fail, Summary: fmt.Sprintf("read default branch head: %v", err), Err: err}
	}
	var workflows struct {
		Workflows []struct {
			ID   int64  `json:"id"`
			Path string `json:"path"`
		} `json:"workflows"`
	}
	if err := githubGET(ctx, c.APIBase, token, "repos/"+c.GitHub.TestRepository+"/actions/workflows", &workflows); err != nil {
		return Result{Status: Fail, Summary: fmt.Sprintf("read integration workflows: %v", err), Err: err}
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
		return Result{Status: Fail, Summary: fmt.Sprintf("read integration workflow runs: %v", err), Err: err}
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
		return Result{Status: Fail, Summary: "pinned upstream commit is unavailable from the configured no-mistakes upstream repository", Err: err}
	}
	var forkCommit struct {
		SHA string `json:"sha"`
	}
	if err := githubGET(ctx, c.APIBase, token, "repos/"+c.NoMistakes.ForkRepository+"/git/commits/"+c.NoMistakes.ForkCommit, &forkCommit); err != nil || forkCommit.SHA != c.NoMistakes.ForkCommit {
		return Result{Status: Fail, Summary: "pinned fork commit is unavailable from the configured no-mistakes fork repository", Err: err}
	}
	var comparison struct {
		Status          string `json:"status"`
		BehindBy        int    `json:"behind_by"`
		MergeBaseCommit struct {
			SHA string `json:"sha"`
		} `json:"merge_base_commit"`
	}
	comparePath := "repos/" + c.NoMistakes.ForkRepository + "/compare/" + c.NoMistakes.UpstreamCommit + "..." + c.NoMistakes.ForkCommit
	if err := githubGET(ctx, c.APIBase, token, comparePath, &comparison); err != nil ||
		comparison.MergeBaseCommit.SHA != c.NoMistakes.UpstreamCommit || comparison.BehindBy != 0 ||
		(comparison.Status != "ahead" && comparison.Status != "identical") {
		return Result{Status: Fail, Summary: "pinned upstream commit is not the merge base of the pinned no-mistakes fork commit", Err: err}
	}
	var forkRelease struct {
		TargetCommitish string `json:"target_commitish"`
		Immutable       bool   `json:"immutable"`
		Assets          []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"assets"`
	}
	if err := githubGET(ctx, c.APIBase, token, "repos/"+c.NoMistakes.ForkRepository+"/releases/tags/"+c.NoMistakes.ForkRelease, &forkRelease); err != nil ||
		forkRelease.TargetCommitish != c.NoMistakes.ForkCommit {
		return Result{Status: Fail, Summary: "fork release target does not equal the pinned fork commit", Err: err}
	}
	if !forkRelease.Immutable {
		return Result{Status: Fail, Summary: "pinned no-mistakes fork release is not immutable"}
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
		return Result{Status: Fail, Summary: fmt.Sprintf("download pinned no-mistakes Linux asset: %v", err), Err: err}
	}
	digest := sha256.Sum256(asset)
	if hex.EncodeToString(digest[:]) != c.NoMistakes.LinuxAMD64SHA256 {
		return Result{Status: Fail, Summary: "pinned no-mistakes Linux asset checksum does not match"}
	}
	return Result{Status: Pass, Summary: "integration workflow, upstream-to-fork ancestry, pinned fork release, and Linux asset checksum verified; owner-only merge remains the governance boundary"}
}

func requirePublicProvenanceRepository(ctx context.Context, apiBase, token, repository, name string) *Result {
	var target struct {
		Private bool `json:"private"`
	}
	if err := githubGET(ctx, apiBase, token, "repos/"+repository, &target); err != nil {
		return &Result{Status: Fail, Summary: fmt.Sprintf("read %s repository: %v", name, err), Err: err}
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
