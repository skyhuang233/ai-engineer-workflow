package doctor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skyhuang233/workflow/internal/credential"
	"github.com/skyhuang233/workflow/internal/store"
)

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
		case r.URL.Path == "/repos/skyhuang233/no-mistakes/releases/assets/9":
			_, _ = w.Write(asset)
		case strings.HasSuffix(r.URL.Path, "/actions/workflows"):
			_, _ = w.Write([]byte(`{"workflows":[{"id":7,"name":"workflow-contract","path":".github/workflows/workflow-contract.yml"}]}`))
		case strings.HasSuffix(r.URL.Path, "/actions/workflows/7/runs"):
			_ = json.NewEncoder(w).Encode(map[string]any{"workflow_runs": []map[string]string{{"head_sha": "current", "status": "completed", "conclusion": "success"}}})
		case strings.Contains(r.URL.Path, "/releases/tags/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"target_commitish": config.NoMistakes.UpstreamCommit, "immutable": immutableRelease, "assets": []map[string]any{{"id": 9, "name": "no-mistakes-" + config.NoMistakes.Version + "-linux-amd64.tar.gz"}}})
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
			_ = json.NewEncoder(w).Encode(map[string]any{"target_commitish": config.NoMistakes.UpstreamCommit, "immutable": true, "assets": []map[string]any{{"id": 9, "name": "no-mistakes-" + config.NoMistakes.Version + "-linux-amd64.tar.gz"}}})
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
