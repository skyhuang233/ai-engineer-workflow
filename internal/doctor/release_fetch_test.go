package doctor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReleaseFetcherProvesManifestReleaseAndPublisherRun(t *testing.T) {
	config := validConfig()
	manifest := `{"schema_version":1,"worker_version":"0.1.0","source_commit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","image":"ghcr.io/skyhuang233/workflow-worker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","codex_version":"0.146.0","no_mistakes_version":"v1.41.2","no_mistakes_commit":"867d64d9c2df89f3f204ad1f5528e5bf7b460caa","github_actions_run_id":123}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/skyhuang233/workflow/releases/tags/worker-v0.1.0":
			_, _ = w.Write([]byte(`{"target_commitish":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","assets":[{"id":9,"name":"worker-release.json"}]}`))
		case "/repos/skyhuang233/workflow/releases/assets/9":
			_, _ = w.Write([]byte(manifest))
		case "/repos/skyhuang233/workflow/actions/runs/123":
			_, _ = w.Write([]byte(`{"head_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","head_branch":"main","event":"push","status":"completed","conclusion":"success"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	got, raw, err := (ReleaseFetcher{APIBase: server.URL, HTTP: server.Client()}).Fetch(context.Background(), config, "github_pat_test")
	if err != nil {
		t.Fatal(err)
	}
	if got.SourceCommit != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" || string(raw) != manifest {
		t.Fatalf("fetch = %#v, %s", got, raw)
	}
}
