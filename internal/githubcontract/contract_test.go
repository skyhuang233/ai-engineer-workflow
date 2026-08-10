package githubcontract

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestVerifyExercisesEveryGatewayPermissionAndCleansUpInPrivateRepository(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer github_pat_test" {
			t.Error("missing in-memory bearer credential")
		}
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/user":
			_, _ = w.Write([]byte(`{"login":"owner"}`))
		case r.URL.Path == "/repos/owner/integration":
			_, _ = w.Write([]byte(`{"full_name":"owner/integration","owner":{"login":"owner"},"default_branch":"main","private":true}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls"):
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues"):
			_, _ = w.Write([]byte(`[]`))
		case r.URL.Path == "/repos/owner/integration/git/ref/heads/main":
			_, _ = w.Write([]byte(`{"object":{"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/integration/commits/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/check-runs":
			_, _ = w.Write([]byte(`{"total_count":0,"check_runs":[]}`))
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/contents/"):
			_, _ = w.Write([]byte(`{"commit":{"sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues"):
			_, _ = w.Write([]byte(`{"number":12}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
			_, _ = w.Write([]byte(`{"number":13}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues/12/labels"):
			_, _ = w.Write([]byte(`[{"name":"workflow-credential-contract"}]`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/git/ref/heads/"):
			_, _ = w.Write([]byte(`{"object":{"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}`))
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{})
		}
	}))
	defer server.Close()

	gitPushes := 0
	err := (Verifier{APIBase: server.URL, Client: server.Client(), Push: func(_ context.Context, token, repository, defaultBranch string) (GitPushArtifact, error) {
		gitPushes++
		if token != "github_pat_test" || repository != "owner/integration" || defaultBranch != "main" {
			t.Fatalf("Git push contract inputs = %q, %q, %q", token, repository, defaultBranch)
		}
		return GitPushArtifact{Branch: contractBranchPrefix + "0123456789abcdef01234567", Commit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, nil
	}}).Verify(
		context.Background(), "github_pat_test", "owner", "owner/integration",
	)
	if err != nil {
		t.Fatal(err)
	}
	if gitPushes != 1 {
		t.Fatalf("Git push lifecycle = %d pushes", gitPushes)
	}
	joined := strings.Join(calls, "\n")
	for _, wanted := range []string{
		"GET /user",
		"GET /repos/owner/integration/actions/workflows",
		"GET /repos/owner/integration/commits/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/check-runs",
		"POST /repos/owner/integration/issues",
		"POST /repos/owner/integration/labels",
		"POST /repos/owner/integration/issues/12/labels",
		"POST /repos/owner/integration/pulls",
		"POST /repos/owner/integration/issues/13/comments",
		"PATCH /repos/owner/integration/pulls/13",
		"PATCH /repos/owner/integration/issues/12",
		"GET /repos/owner/integration/git/ref/heads/workflow-credential-contract-0123456789abcdef01234567",
		"DELETE /repos/owner/integration/git/refs/heads/workflow-credential-contract-0123456789abcdef01234567",
	} {
		if !strings.Contains(joined, wanted) {
			t.Fatalf("missing %q in calls:\n%s", wanted, joined)
		}
	}
}

func TestVerifyRejectsCredentialWithoutChecksReadAfterCandidatePush(t *testing.T) {
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/user":
			_, _ = w.Write([]byte(`{"login":"owner"}`))
		case "/repos/owner/integration":
			_, _ = w.Write([]byte(`{"full_name":"owner/integration","owner":{"login":"owner"},"default_branch":"main","private":true}`))
		case "/repos/owner/integration/actions/workflows":
			_, _ = w.Write([]byte(`{"workflows":[]}`))
		case "/repos/owner/integration/commits/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/check-runs":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"Resource not accessible by personal access token"}`))
		case "/repos/owner/integration/git/ref/heads/workflow-credential-contract-0123456789abcdef01234567":
			_, _ = w.Write([]byte(`{"object":{"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}`))
		case "/repos/owner/integration/git/refs/heads/workflow-credential-contract-0123456789abcdef01234567":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	err := (Verifier{APIBase: server.URL, Client: server.Client(), Push: func(context.Context, string, string, string) (GitPushArtifact, error) {
		calls = append(calls, "PUSH workflow-credential-contract-0123456789abcdef01234567@aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
		return GitPushArtifact{Branch: contractBranchPrefix + "0123456789abcdef01234567", Commit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, nil
	}}).Verify(context.Background(), "github_pat_actions_only", "owner", "owner/integration")
	if err == nil || !strings.Contains(err.Error(), "verify Checks read permission") || !strings.Contains(err.Error(), "403") {
		t.Fatalf("missing Checks read rejection = %v", err)
	}
	joined := strings.Join(calls, "\n")
	for _, wanted := range []string{
		"GET /repos/owner/integration/actions/workflows",
		"PUSH workflow-credential-contract-0123456789abcdef01234567@aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"GET /repos/owner/integration/commits/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/check-runs",
		"DELETE /repos/owner/integration/git/refs/heads/workflow-credential-contract-0123456789abcdef01234567",
	} {
		if !strings.Contains(joined, wanted) {
			t.Fatalf("missing %q in calls:\n%s", wanted, joined)
		}
	}
	for _, forbidden := range []string{"/issues", "/pulls", "/labels"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("credential contract continued to %q after Checks read rejection:\n%s", forbidden, joined)
		}
	}
	t.Logf("Actions read succeeded, temporary Candidate %s was pushed, check-runs returned 403, credential admission stopped, and the temporary branch was deleted.\nAPI transcript:\n%s\nVerifier result: %v", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", joined, err)
}

func TestVerifyRejectsCanonicalRepositoryOwnedByAnotherAccountBeforeMutations(t *testing.T) {
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/user":
			_, _ = w.Write([]byte(`{"login":"owner"}`))
		case "/repos/owner/integration":
			_, _ = w.Write([]byte(`{"full_name":"collaborator/integration","owner":{"login":"collaborator"},"default_branch":"main","private":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	err := (Verifier{APIBase: server.URL, Client: server.Client()}).Verify(
		context.Background(), "github_pat_test", "owner", "owner/integration",
	)
	if err == nil || !strings.Contains(err.Error(), "does not match configured owner") {
		t.Fatalf("canonical repository owner admission error = %v", err)
	}
	if got := strings.Join(calls, "\n"); strings.Contains(got, "POST ") || strings.Contains(got, "PUT ") || strings.Contains(got, "PATCH ") || strings.Contains(got, "DELETE ") {
		t.Fatalf("canonical owner mismatch mutated repository:\n%s", got)
	}
}

func TestVerifyRejectsRepositoryOwnedByAnotherAccountBeforeMutations(t *testing.T) {
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/user":
			_, _ = w.Write([]byte(`{"login":"owner"}`))
		case "/repos/owner/integration":
			_, _ = w.Write([]byte(`{"default_branch":"main","private":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	err := (Verifier{APIBase: server.URL, Client: server.Client()}).Verify(
		context.Background(), "github_pat_test", "owner", "collaborator/integration",
	)
	if err == nil || !strings.Contains(err.Error(), "does not match configured owner") {
		t.Fatalf("repository owner admission error = %v", err)
	}
	if got := strings.Join(calls, "\n"); strings.Contains(got, "POST ") || strings.Contains(got, "PUT ") || strings.Contains(got, "PATCH ") || strings.Contains(got, "DELETE ") {
		t.Fatalf("private integration repository was mutated:\n%s", got)
	}
}

func TestVerifyRefusesToDeleteAChangedTemporaryBranch(t *testing.T) {
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/user":
			_, _ = w.Write([]byte(`{"login":"owner"}`))
		case "/repos/owner/integration":
			_, _ = w.Write([]byte(`{"full_name":"owner/integration","owner":{"login":"owner"},"default_branch":"main"}`))
		case "/repos/owner/integration/git/ref/heads/workflow-credential-contract-0123456789abcdef01234567":
			_, _ = w.Write([]byte(`{"object":{"sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}`))
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{})
		}
	}))
	defer server.Close()

	err := (Verifier{APIBase: server.URL, Client: server.Client(), Push: func(context.Context, string, string, string) (GitPushArtifact, error) {
		return GitPushArtifact{Branch: contractBranchPrefix + "0123456789abcdef01234567", Commit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, nil
	}}).Verify(context.Background(), "github_pat_test", "owner", "owner/integration")
	if err == nil || !strings.Contains(err.Error(), "head changed") {
		t.Fatalf("Verify error = %v", err)
	}
	if strings.Contains(strings.Join(calls, "\n"), "DELETE /repos/owner/integration/git/refs/heads/") {
		t.Fatalf("deleted changed branch:\n%s", strings.Join(calls, "\n"))
	}
}
