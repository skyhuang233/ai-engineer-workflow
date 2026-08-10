package doctor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skyhuang233/workflow/internal/credential"
	"github.com/skyhuang233/workflow/internal/store"
)

type dockerCheckExecutor struct {
	commands [][]string
	metadata []byte
}

func (e *dockerCheckExecutor) Run(_ context.Context, command []string) ([]byte, error) {
	e.commands = append(e.commands, append([]string(nil), command...))
	joined := strings.Join(command, " ")
	switch {
	case joined == "docker info --format {{.OSType}}/{{.Architecture}}":
		return []byte("linux/x86_64\n"), nil
	case strings.HasPrefix(joined, "docker pull "):
		return []byte("pulled\n"), nil
	case strings.Contains(joined, " sh -ceu "):
		for _, arg := range command {
			const prefix = "type=bind,src="
			const suffix = ",dst=/workspace"
			if strings.HasPrefix(arg, prefix) && strings.HasSuffix(arg, suffix) {
				workspace := strings.TrimSuffix(strings.TrimPrefix(arg, prefix), suffix)
				if err := os.WriteFile(filepath.Join(workspace, "container-marker"), []byte("worker"), 0o600); err != nil {
					return nil, err
				}
			}
		}
		return []byte("gateway=ok\nmount=ok\n0.147.0\ngh version 2.97.0\ngo1.25.12\nv1.41.2\n\tbuild\tvcs.revision=e073fd0dc51c64004468b04de8cf2ab50cd5d177\n\tbuild\tvcs.modified=false\n"), nil
	case strings.Contains(joined, " --entrypoint /usr/local/go/bin/go "):
		return e.metadata, nil
	default:
		return nil, nil
	}
}

func TestWorkerNoMistakesBuildMetadataRequiresExactCleanForkRevision(t *testing.T) {
	const forkCommit = "e073fd0dc51c64004468b04de8cf2ab50cd5d177"
	tests := []struct {
		name     string
		metadata string
		wantErr  string
	}{
		{
			name:     "exact clean fork",
			metadata: "\tbuild\tvcs.revision=" + forkCommit + "\n\tbuild\tvcs.modified=false\n",
		},
		{
			name:     "upstream revision",
			metadata: "\tbuild\tvcs.revision=867d64d9c2df89f3f204ad1f5528e5bf7b460caa\n\tbuild\tvcs.modified=false\n",
			wantErr:  "does not equal pinned fork commit",
		},
		{
			name:     "modified fork build",
			metadata: "\tbuild\tvcs.revision=" + forkCommit + "\n\tbuild\tvcs.modified=true\n",
			wantErr:  "is not a clean build",
		},
		{
			name:     "missing metadata",
			metadata: "no VCS settings",
			wantErr:  "does not equal pinned fork commit",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := verifyWorkerNoMistakesBuildMetadata(test.metadata, forkCommit)
			if test.wantErr == "" && err != nil {
				t.Fatalf("verifyWorkerNoMistakesBuildMetadata() error = %v", err)
			}
			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("verifyWorkerNoMistakesBuildMetadata() error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestDockerCheckRejectsBuildMetadataFromOtherProbeOutput(t *testing.T) {
	const forkCommit = "e073fd0dc51c64004468b04de8cf2ab50cd5d177"
	executor := &dockerCheckExecutor{metadata: []byte("/usr/local/bin/no-mistakes: go1.25.12\n")}
	result := (DockerCheck{
		Executor: executor,
		Manifest: WorkerReleaseManifest{
			Image:                "ghcr.io/owner/worker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			CodexVersion:         "0.147.0",
			GitHubCLIVersion:     "2.97.0",
			GoVersion:            "1.25.12",
			NoMistakesVersion:    "v1.41.2",
			NoMistakesForkCommit: forkCommit,
		},
	}).Run(context.Background())

	if result.Status != Fail || !strings.Contains(result.Summary, "does not equal pinned fork commit") {
		t.Fatalf("DockerCheck.Run() = %#v, want isolated metadata failure", result)
	}
	if len(executor.commands) != 4 {
		t.Fatalf("DockerCheck command count = %d, want 4", len(executor.commands))
	}
	probeScript := executor.commands[2][len(executor.commands[2])-1]
	if strings.Contains(probeScript, "go version -m") {
		t.Fatalf("general Worker probe contains build metadata command: %q", probeScript)
	}
	for _, required := range []string{"no-mistakes daemon start", "no-mistakes daemon status"} {
		if !strings.Contains(probeScript, required) {
			t.Fatalf("general Worker probe omits Delivery Controller runtime check %q: %q", required, probeScript)
		}
	}
	if command := strings.Join(executor.commands[2], " "); !strings.Contains(command, "--env NM_HOME=/codex-state/no-mistakes") {
		t.Fatalf("general Worker probe omits persistent no-mistakes home: %q", command)
	}
	metadataCommand := strings.Join(executor.commands[3], " ")
	wantMetadataCommand := "docker run --rm --entrypoint /usr/local/go/bin/go ghcr.io/owner/worker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb version -m /usr/local/bin/no-mistakes"
	if metadataCommand != wantMetadataCommand {
		t.Fatalf("metadata command = %q, want %q", metadataCommand, wantMetadataCommand)
	}
}

func TestSQLiteCheckReportsBackupAndRecoveryMetrics(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "workflow.db")
	database, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	backupPath := databasePath + ".backup"
	if _, err := database.CreateOnlineBackup(ctx, backupPath, time.Now().UTC()); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if _, err := store.DrillBackup(ctx, backupPath, time.Now().UTC()); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	result := (SQLiteCheck{Path: databasePath, BackupPath: backupPath}).Run(ctx)
	if result.Status != Pass || !strings.Contains(result.Summary, "backup_age=") || !strings.Contains(result.Summary, "drill_succeeded=true") || !strings.Contains(result.Summary, "outbox_age=") || !strings.Contains(result.Summary, "reconcile_lag=") {
		t.Fatalf("SQLite doctor backup metrics = %#v", result)
	}
}

type memoryCredential struct{ secret string }

func (m memoryCredential) Get(context.Context, string) (string, error) { return m.secret, nil }
func (m memoryCredential) Set(context.Context, string, string) error   { return nil }

func TestGitHubChecksUseOwnerGuardedReadOnlyContractWithoutBranchProtection(t *testing.T) {
	config := validConfig()
	token := "github_pat_test"
	asset := []byte("pinned no-mistakes asset")
	digest := sha256.Sum256(asset)
	config.NoMistakes.LinuxAMD64SHA256 = hex.EncodeToString(digest[:])
	var paths []string
	immutableRelease := true
	forkIsBasedOnUpstream := true
	releaseTargetsFork := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/user":
			_, _ = w.Write([]byte(`{"login":"skyhuang233"}`))
		case r.URL.Path == "/repos/skyhuang233/workflow-integration-test":
			_, _ = w.Write([]byte(`{"full_name":"skyhuang233/workflow-integration-test","owner":{"login":"skyhuang233"},"default_branch":"main","private":true}`))
		case r.URL.Path == "/repos/skyhuang233/workflow-integration-test/git/ref/heads/main":
			_, _ = w.Write([]byte(`{"object":{"sha":"current"}}`))
		case r.URL.Path == "/repos/skyhuang233/no-mistakes":
			_, _ = w.Write([]byte(`{"private":false}`))
		case r.URL.Path == "/repos/kunchenguid/no-mistakes":
			_, _ = w.Write([]byte(`{"private":false}`))
		case r.URL.Path == "/repos/kunchenguid/no-mistakes/git/commits/"+config.NoMistakes.UpstreamCommit:
			_ = json.NewEncoder(w).Encode(map[string]string{"sha": config.NoMistakes.UpstreamCommit})
		case r.URL.Path == "/repos/skyhuang233/no-mistakes/git/commits/"+config.NoMistakes.ForkCommit:
			_ = json.NewEncoder(w).Encode(map[string]string{"sha": config.NoMistakes.ForkCommit})
		case r.URL.Path == "/repos/skyhuang233/no-mistakes/compare/"+config.NoMistakes.UpstreamCommit+"..."+config.NoMistakes.ForkCommit:
			mergeBase := config.NoMistakes.UpstreamCommit
			if !forkIsBasedOnUpstream {
				mergeBase = strings.Repeat("f", 40)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ahead", "behind_by": 0, "merge_base_commit": map[string]string{"sha": mergeBase}})
		case r.URL.Path == "/repos/skyhuang233/no-mistakes/releases/assets/9":
			_, _ = w.Write(asset)
		case strings.HasSuffix(r.URL.Path, "/actions/workflows"):
			_, _ = w.Write([]byte(`{"workflows":[{"id":7,"name":"workflow-contract","path":".github/workflows/workflow-contract.yml"}]}`))
		case strings.HasSuffix(r.URL.Path, "/actions/workflows/7/runs"):
			_ = json.NewEncoder(w).Encode(map[string]any{"workflow_runs": []map[string]string{{"head_sha": "current", "status": "completed", "conclusion": "success"}}})
		case strings.Contains(r.URL.Path, "/releases/tags/"):
			target := config.NoMistakes.ForkCommit
			if !releaseTargetsFork {
				target = config.NoMistakes.UpstreamCommit
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"target_commitish": target, "immutable": immutableRelease, "assets": []map[string]any{{"id": 9, "name": "no-mistakes-" + config.NoMistakes.Version + "-linux-amd64.tar.gz"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	credentials := memoryCredential{secret: token}
	verification := store.GatewayCredentialVerification{
		FingerprintSHA256:     credential.Fingerprint(token),
		Owner:                 config.GitHub.Credential.Owner,
		IntegrationRepository: config.GitHub.TestRepository,
	}
	if result := (GitHubCredentialCheck{
		Pin: config.GitHub.Credential, IntegrationRepository: config.GitHub.TestRepository, Credentials: credentials,
		Verification: verification, APIBase: server.URL,
	}).Run(context.Background()); result.Status != Pass {
		t.Fatalf("credential check = %#v", result)
	}
	if result := (GitHubCheck{
		GitHub: config.GitHub, NoMistakes: config.NoMistakes,
		Credentials: credentials, APIBase: server.URL,
	}).Run(context.Background()); result.Status != Pass {
		t.Fatalf("GitHub check = %#v", result)
	}
	immutableRelease = false
	if result := (GitHubCheck{GitHub: config.GitHub, NoMistakes: config.NoMistakes, Credentials: credentials, APIBase: server.URL}).Run(context.Background()); result.Status != Fail || !strings.Contains(result.Summary, "immutable") {
		t.Fatalf("GitHub check accepted a mutable no-mistakes release: %#v", result)
	}
	immutableRelease = true
	releaseTargetsFork = false
	if result := (GitHubCheck{GitHub: config.GitHub, NoMistakes: config.NoMistakes, Credentials: credentials, APIBase: server.URL}).Run(context.Background()); result.Status != Fail || !strings.Contains(result.Summary, "fork release target") {
		t.Fatalf("GitHub check accepted a release targeting the upstream instead of the fork: %#v", result)
	}
	releaseTargetsFork = true
	forkIsBasedOnUpstream = false
	if result := (GitHubCheck{GitHub: config.GitHub, NoMistakes: config.NoMistakes, Credentials: credentials, APIBase: server.URL}).Run(context.Background()); result.Status != Fail || !strings.Contains(result.Summary, "merge base") {
		t.Fatalf("GitHub check accepted a fork unrelated to the pinned upstream commit: %#v", result)
	}
	forkIsBasedOnUpstream = true
	asset = []byte("tampered no-mistakes asset")
	if result := (GitHubCheck{GitHub: config.GitHub, NoMistakes: config.NoMistakes, Credentials: credentials, APIBase: server.URL}).Run(context.Background()); result.Status != Fail || !strings.Contains(result.Summary, "checksum") {
		t.Fatalf("GitHub check accepted tampered no-mistakes asset: %#v", result)
	}
	for _, path := range paths {
		if strings.Contains(path, "protection") {
			t.Fatalf("Owner-Guarded doctor queried branch protection: %s", path)
		}
	}
}

func TestGitHubCheckRejectsCanonicalIntegrationRepositoryOwnedByAnotherAccount(t *testing.T) {
	config := validConfig()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/repos/skyhuang233/workflow-integration-test" {
			_, _ = w.Write([]byte(`{"full_name":"collaborator/workflow-integration-test","owner":{"login":"collaborator"},"default_branch":"main","private":true}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	result := (GitHubCheck{
		GitHub: config.GitHub, NoMistakes: config.NoMistakes,
		Credentials: memoryCredential{secret: "github_pat_test"}, APIBase: server.URL,
	}).Run(context.Background())
	if result.Status != Fail || !strings.Contains(result.Summary, "does not match configured owner") {
		t.Fatalf("GitHub check canonical owner admission = %#v", result)
	}
}

func TestGitHubCheckRejectsUnavailablePinnedUpstreamCommit(t *testing.T) {
	config := validConfig()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/repos/skyhuang233/workflow-integration-test":
			_, _ = w.Write([]byte(`{"full_name":"skyhuang233/workflow-integration-test","owner":{"login":"skyhuang233"},"default_branch":"main","private":false}`))
		case r.URL.Path == "/repos/skyhuang233/workflow-integration-test/git/ref/heads/main":
			_, _ = w.Write([]byte(`{"object":{"sha":"current"}}`))
		case strings.HasSuffix(r.URL.Path, "/actions/workflows"):
			_ = json.NewEncoder(w).Encode(map[string]any{"workflows": []map[string]any{{"id": 7, "path": config.GitHub.WorkflowPath}}})
		case strings.HasSuffix(r.URL.Path, "/actions/workflows/7/runs"):
			_ = json.NewEncoder(w).Encode(map[string]any{"workflow_runs": []map[string]string{{"head_sha": "current", "status": "completed", "conclusion": "success"}}})
		case r.URL.Path == "/repos/kunchenguid/no-mistakes", r.URL.Path == "/repos/skyhuang233/no-mistakes":
			_, _ = w.Write([]byte(`{"private":false}`))
		case strings.Contains(r.URL.Path, "/releases/tags/"):
			_ = json.NewEncoder(w).Encode(map[string]string{"target_commitish": config.NoMistakes.UpstreamCommit})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	result := (GitHubCheck{GitHub: config.GitHub, NoMistakes: config.NoMistakes, Credentials: memoryCredential{secret: "github_pat_test"}, APIBase: server.URL}).Run(context.Background())
	if result.Status != Fail || !strings.Contains(result.Summary, "pinned upstream commit") {
		t.Fatalf("GitHub check = %#v, want unavailable upstream commit failure", result)
	}
}

func TestGitHubCredentialCheckRejectsDifferentIntegrationRepository(t *testing.T) {
	config := validConfig()
	token := "github_pat_test"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"login":"skyhuang233"}`))
	}))
	defer server.Close()
	result := (GitHubCredentialCheck{
		Pin: config.GitHub.Credential, IntegrationRepository: config.GitHub.TestRepository,
		Credentials: memoryCredential{secret: token},
		Verification: store.GatewayCredentialVerification{
			FingerprintSHA256: credential.Fingerprint(token), Owner: config.GitHub.Credential.Owner,
			IntegrationRepository: "skyhuang233/different-integration",
		},
		APIBase: server.URL,
	}).Run(context.Background())
	if result.Status != Fail || !strings.Contains(result.Summary, "integration repository") {
		t.Fatalf("credential check = %#v", result)
	}
}

func TestGitHubCheckRejectsContractRunFromAnOldDefaultHead(t *testing.T) {
	config := validConfig()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/repos/skyhuang233/workflow-integration-test":
			_, _ = w.Write([]byte(`{"full_name":"skyhuang233/workflow-integration-test","owner":{"login":"skyhuang233"},"default_branch":"main","private":false}`))
		case r.URL.Path == "/repos/skyhuang233/workflow-integration-test/git/ref/heads/main":
			_, _ = w.Write([]byte(`{"object":{"sha":"current"}}`))
		case r.URL.Path == "/repos/skyhuang233/no-mistakes":
			_, _ = w.Write([]byte(`{"private":false}`))
		case strings.HasSuffix(r.URL.Path, "/actions/workflows"):
			_, _ = w.Write([]byte(`{"workflows":[{"id":7,"name":"workflow-contract","path":".github/workflows/workflow-contract.yml"}]}`))
		case strings.HasSuffix(r.URL.Path, "/actions/workflows/7/runs"):
			_ = json.NewEncoder(w).Encode(map[string]any{"workflow_runs": []map[string]string{{"head_sha": "old", "status": "completed", "conclusion": "success"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	result := (GitHubCheck{
		GitHub: config.GitHub, NoMistakes: config.NoMistakes,
		Credentials: memoryCredential{secret: "github_pat_test"}, APIBase: server.URL,
	}).Run(context.Background())
	if result.Status != Fail || !strings.Contains(result.Summary, "current default-branch revision") {
		t.Fatalf("GitHub check = %#v, want stale-run failure", result)
	}
}

func TestGitHubCheckRejectsPrivateNoMistakesFork(t *testing.T) {
	config := validConfig()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/repos/skyhuang233/workflow-integration-test":
			_, _ = w.Write([]byte(`{"full_name":"skyhuang233/workflow-integration-test","owner":{"login":"skyhuang233"},"default_branch":"main","private":false}`))
		case r.URL.Path == "/repos/skyhuang233/workflow-integration-test/git/ref/heads/main":
			_, _ = w.Write([]byte(`{"object":{"sha":"current"}}`))
		case strings.HasSuffix(r.URL.Path, "/actions/workflows"):
			_ = json.NewEncoder(w).Encode(map[string]any{"workflows": []map[string]any{{"id": 7, "path": config.GitHub.WorkflowPath}}})
		case strings.HasSuffix(r.URL.Path, "/actions/workflows/7/runs"):
			_ = json.NewEncoder(w).Encode(map[string]any{"workflow_runs": []map[string]string{{"head_sha": "current", "status": "completed", "conclusion": "success"}}})
		case r.URL.Path == "/repos/skyhuang233/no-mistakes":
			_, _ = w.Write([]byte(`{"private":true}`))
		case r.URL.Path == "/repos/kunchenguid/no-mistakes":
			_, _ = w.Write([]byte(`{"private":false}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	result := (GitHubCheck{GitHub: config.GitHub, NoMistakes: config.NoMistakes, Credentials: memoryCredential{secret: "github_pat_test"}, APIBase: server.URL}).Run(context.Background())
	if result.Status != Fail || !strings.Contains(result.Summary, "must be public") {
		t.Fatalf("GitHub check = %#v, want private fork failure", result)
	}
}

func TestGitHubCheckRejectsPrivateNoMistakesUpstream(t *testing.T) {
	config := validConfig()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/repos/skyhuang233/workflow-integration-test":
			_, _ = w.Write([]byte(`{"full_name":"skyhuang233/workflow-integration-test","owner":{"login":"skyhuang233"},"default_branch":"main","private":false}`))
		case r.URL.Path == "/repos/skyhuang233/workflow-integration-test/git/ref/heads/main":
			_, _ = w.Write([]byte(`{"object":{"sha":"current"}}`))
		case strings.HasSuffix(r.URL.Path, "/actions/workflows"):
			_ = json.NewEncoder(w).Encode(map[string]any{"workflows": []map[string]any{{"id": 7, "path": config.GitHub.WorkflowPath}}})
		case strings.HasSuffix(r.URL.Path, "/actions/workflows/7/runs"):
			_ = json.NewEncoder(w).Encode(map[string]any{"workflow_runs": []map[string]string{{"head_sha": "current", "status": "completed", "conclusion": "success"}}})
		case r.URL.Path == "/repos/kunchenguid/no-mistakes":
			_, _ = w.Write([]byte(`{"private":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	result := (GitHubCheck{GitHub: config.GitHub, NoMistakes: config.NoMistakes, Credentials: memoryCredential{secret: "github_pat_test"}, APIBase: server.URL}).Run(context.Background())
	if result.Status != Fail || !strings.Contains(result.Summary, "upstream repository must be public") {
		t.Fatalf("GitHub check = %#v, want private upstream failure", result)
	}
}

func TestGitHubCheckPinsTheIntegrationWorkflowByConfiguredPath(t *testing.T) {
	config := validConfig()
	asset := []byte("pinned no-mistakes asset")
	digest := sha256.Sum256(asset)
	config.NoMistakes.LinuxAMD64SHA256 = hex.EncodeToString(digest[:])
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/repos/skyhuang233/workflow-integration-test":
			_, _ = w.Write([]byte(`{"full_name":"skyhuang233/workflow-integration-test","owner":{"login":"skyhuang233"},"default_branch":"main","private":false}`))
		case r.URL.Path == "/repos/skyhuang233/workflow-integration-test/git/ref/heads/main":
			_, _ = w.Write([]byte(`{"object":{"sha":"current"}}`))
		case r.URL.Path == "/repos/skyhuang233/no-mistakes":
			_, _ = w.Write([]byte(`{"private":false}`))
		case r.URL.Path == "/repos/kunchenguid/no-mistakes":
			_, _ = w.Write([]byte(`{"private":false}`))
		case r.URL.Path == "/repos/kunchenguid/no-mistakes/git/commits/"+config.NoMistakes.UpstreamCommit:
			_ = json.NewEncoder(w).Encode(map[string]string{"sha": config.NoMistakes.UpstreamCommit})
		case r.URL.Path == "/repos/skyhuang233/no-mistakes/git/commits/"+config.NoMistakes.ForkCommit:
			_ = json.NewEncoder(w).Encode(map[string]string{"sha": config.NoMistakes.ForkCommit})
		case r.URL.Path == "/repos/skyhuang233/no-mistakes/compare/"+config.NoMistakes.UpstreamCommit+"..."+config.NoMistakes.ForkCommit:
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ahead", "behind_by": 0, "merge_base_commit": map[string]string{"sha": config.NoMistakes.UpstreamCommit}})
		case r.URL.Path == "/repos/skyhuang233/no-mistakes/releases/assets/9":
			_, _ = w.Write(asset)
		case strings.HasSuffix(r.URL.Path, "/actions/workflows"):
			_ = json.NewEncoder(w).Encode(map[string]any{"workflows": []map[string]any{
				{"id": 7, "name": config.GitHub.RequiredCheck, "path": ".github/workflows/unrelated.yml"},
				{"id": 8, "name": "renamed-contract", "path": config.GitHub.WorkflowPath},
			}})
		case strings.HasSuffix(r.URL.Path, "/actions/workflows/8/runs"):
			_ = json.NewEncoder(w).Encode(map[string]any{"workflow_runs": []map[string]string{{"head_sha": "current", "status": "completed", "conclusion": "success"}}})
		case strings.Contains(r.URL.Path, "/releases/tags/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"target_commitish": config.NoMistakes.ForkCommit, "immutable": true, "assets": []map[string]any{{"id": 9, "name": "no-mistakes-" + config.NoMistakes.Version + "-linux-amd64.tar.gz"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	result := (GitHubCheck{
		GitHub: config.GitHub, NoMistakes: config.NoMistakes,
		Credentials: memoryCredential{secret: "github_pat_test"}, APIBase: server.URL,
	}).Run(context.Background())
	if result.Status != Pass {
		t.Fatalf("GitHub check = %#v, want configured-path pass", result)
	}
}
