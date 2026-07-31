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

func TestVerifyExercisesEveryGatewayPermissionAndCleansUp(t *testing.T) {
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
			_, _ = w.Write([]byte(`{"default_branch":"main"}`))
		case r.URL.Path == "/repos/owner/integration/git/ref/heads/main":
			_, _ = w.Write([]byte(`{"object":{"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}`))
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/contents/"):
			_, _ = w.Write([]byte(`{"commit":{"sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues"):
			_, _ = w.Write([]byte(`{"number":12}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
			_, _ = w.Write([]byte(`{"number":13}`))
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{})
		}
	}))
	defer server.Close()

	err := (Verifier{APIBase: server.URL, Client: server.Client()}).Verify(
		context.Background(), "github_pat_test", "owner", "owner/integration",
	)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(calls, "\n")
	for _, wanted := range []string{
		"GET /user",
		"GET /repos/owner/integration/actions/workflows",
		"POST /repos/owner/integration/git/refs",
		"PUT /repos/owner/integration/contents/.workflow-credential-contract",
		"POST /repos/owner/integration/issues",
		"POST /repos/owner/integration/labels",
		"POST /repos/owner/integration/issues/12/labels",
		"POST /repos/owner/integration/pulls",
		"POST /repos/owner/integration/issues/13/comments",
		"PATCH /repos/owner/integration/pulls/13",
		"PATCH /repos/owner/integration/issues/12",
		"DELETE /repos/owner/integration/git/refs/heads/workflow-credential-contract",
	} {
		if !strings.Contains(joined, wanted) {
			t.Fatalf("missing %q in calls:\n%s", wanted, joined)
		}
	}
}
