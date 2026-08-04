package doctor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/skyhuang233/workflow/internal/credential"
	"github.com/skyhuang233/workflow/internal/store"
)

type memoryCredential struct{ secret string }

func (m memoryCredential) Get(context.Context, string) (string, error) { return m.secret, nil }
func (m memoryCredential) Set(context.Context, string, string) error   { return nil }

func TestGitHubChecksUseOwnerGuardedReadOnlyContractWithoutBranchProtection(t *testing.T) {
	config := validConfig()
	token := "github_pat_test"
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/user":
			_, _ = w.Write([]byte(`{"login":"skyhuang233"}`))
		case r.URL.Path == "/repos/skyhuang233/workflow-integration-test":
			_, _ = w.Write([]byte(`{"default_branch":"main","private":false}`))
		case r.URL.Path == "/repos/skyhuang233/workflow-integration-test/git/ref/heads/main":
			_, _ = w.Write([]byte(`{"object":{"sha":"current"}}`))
		case strings.HasSuffix(r.URL.Path, "/actions/workflows"):
			_, _ = w.Write([]byte(`{"workflows":[{"id":7,"name":"workflow-contract","path":".github/workflows/workflow-contract.yml"}]}`))
		case strings.HasSuffix(r.URL.Path, "/actions/workflows/7/runs"):
			_ = json.NewEncoder(w).Encode(map[string]any{"workflow_runs": []map[string]string{{"head_sha": "current", "status": "completed", "conclusion": "success"}}})
		case strings.Contains(r.URL.Path, "/releases/tags/"):
			_ = json.NewEncoder(w).Encode(map[string]string{"target_commitish": config.NoMistakes.UpstreamCommit})
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
	for _, path := range paths {
		if strings.Contains(path, "protection") {
			t.Fatalf("Owner-Guarded doctor queried branch protection: %s", path)
		}
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
			_, _ = w.Write([]byte(`{"default_branch":"main","private":false}`))
		case r.URL.Path == "/repos/skyhuang233/workflow-integration-test/git/ref/heads/main":
			_, _ = w.Write([]byte(`{"object":{"sha":"current"}}`))
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

func TestGitHubCheckPinsTheIntegrationWorkflowByConfiguredPath(t *testing.T) {
	config := validConfig()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/repos/skyhuang233/workflow-integration-test":
			_, _ = w.Write([]byte(`{"default_branch":"main","private":false}`))
		case r.URL.Path == "/repos/skyhuang233/workflow-integration-test/git/ref/heads/main":
			_, _ = w.Write([]byte(`{"object":{"sha":"current"}}`))
		case strings.HasSuffix(r.URL.Path, "/actions/workflows"):
			_ = json.NewEncoder(w).Encode(map[string]any{"workflows": []map[string]any{
				{"id": 7, "name": config.GitHub.RequiredCheck, "path": ".github/workflows/unrelated.yml"},
				{"id": 8, "name": "renamed-contract", "path": config.GitHub.WorkflowPath},
			}})
		case strings.HasSuffix(r.URL.Path, "/actions/workflows/8/runs"):
			_ = json.NewEncoder(w).Encode(map[string]any{"workflow_runs": []map[string]string{{"head_sha": "current", "status": "completed", "conclusion": "success"}}})
		case strings.Contains(r.URL.Path, "/releases/tags/"):
			_ = json.NewEncoder(w).Encode(map[string]string{"target_commitish": config.NoMistakes.UpstreamCommit})
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
