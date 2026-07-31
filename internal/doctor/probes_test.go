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
			_, _ = w.Write([]byte(`{"default_branch":"main","private":true}`))
		case strings.HasSuffix(r.URL.Path, "/actions/workflows"):
			_, _ = w.Write([]byte(`{"workflows":[{"id":7,"name":"workflow-contract"}]}`))
		case strings.HasSuffix(r.URL.Path, "/actions/workflows/7/runs"):
			_ = json.NewEncoder(w).Encode(map[string]any{"workflow_runs": []map[string]string{{"conclusion": "success"}}})
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
		Pin: config.GitHub.Credential, Credentials: credentials,
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
