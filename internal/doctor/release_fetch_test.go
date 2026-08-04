package doctor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReleaseFetcherProvesManifestReleaseAndPublisherRun(t *testing.T) {
	config := validConfig()
	private := false
	assets := `[{"id":9,"name":"worker-release.json"}]`
	manifest := `{"schema_version":1,"worker_version":"0.1.0","source_commit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","image":"ghcr.io/skyhuang233/workflow-worker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","codex_version":"0.146.0","no_mistakes_version":"v1.41.2","no_mistakes_upstream_repository":"kunchenguid/no-mistakes","no_mistakes_commit":"867d64d9c2df89f3f204ad1f5528e5bf7b460caa","no_mistakes_fork_repository":"skyhuang233/no-mistakes","no_mistakes_fork_release":"workflow-v1.41.2.0","no_mistakes_linux_amd64_sha256":"a100c58bdfe7df9f598ecec32553d5fbd8eb0079912fc830f362011fd9dc8825","github_actions_run_id":123}`
	workflowID := int64(77)
	mainSHA := "cccccccccccccccccccccccccccccccccccccccc"
	mergedBy := "skyhuang233"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/skyhuang233/workflow":
			_, _ = w.Write([]byte(fmt.Sprintf(`{"private":%t}`, private)))
		case "/repos/skyhuang233/workflow/releases/tags/worker-v0.1.0":
			_, _ = w.Write([]byte(`{"target_commitish":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","assets":` + assets + `}`))
		case "/repos/skyhuang233/workflow/releases/assets/9":
			_, _ = w.Write([]byte(manifest))
		case "/repos/skyhuang233/workflow/actions/runs/123":
			_, _ = w.Write([]byte(`{"head_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","head_branch":"main","event":"push","status":"completed","conclusion":"success","workflow_id":` + fmt.Sprint(workflowID) + `}`))
		case "/repos/skyhuang233/workflow/actions/workflows/publish-worker.yml":
			_, _ = w.Write([]byte(`{"id":77,"path":".github/workflows/publish-worker.yml","state":"active"}`))
		case "/repos/skyhuang233/workflow/commits/main":
			_, _ = w.Write([]byte(`{"sha":"` + mainSHA + `"}`))
		case "/repos/skyhuang233/workflow/commits/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/pulls":
			_, _ = w.Write([]byte(`[{"merged_at":"2026-08-01T00:00:00Z","merge_commit_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","base":{"ref":"main"},"merged_by":{"login":"` + mergedBy + `","type":"User"}}]`))
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
	assets = `[{"id":9,"name":"worker-release.json"},{"id":10,"name":"worker-release.json"}]`
	if _, _, err := (ReleaseFetcher{APIBase: server.URL, HTTP: server.Client()}).Fetch(context.Background(), config, "github_pat_test"); err == nil {
		t.Fatal("accepted a release with duplicate worker-release.json assets")
	}
	assets = `[{"id":9,"name":"worker-release.json"}]`
	workflowID = 88
	if _, _, err := (ReleaseFetcher{APIBase: server.URL, HTTP: server.Client()}).Fetch(context.Background(), config, "github_pat_test"); err == nil {
		t.Fatal("accepted a manifest attributed to an unrelated successful workflow")
	}
	workflowID = 77
	mergedBy = "workflow[bot]"
	if _, _, err := (ReleaseFetcher{APIBase: server.URL, HTTP: server.Client()}).Fetch(context.Background(), config, "github_pat_test"); err == nil {
		t.Fatal("accepted a bot-merged release")
	}
	mergedBy = "skyhuang233"
	private = true
	if _, _, err := (ReleaseFetcher{APIBase: server.URL, HTTP: server.Client()}).Fetch(context.Background(), config, "github_pat_test"); err == nil {
		t.Fatal("accepted a private Worker Release repository")
	}
}
