package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReleaseFetcherProvesManifestReleaseAndPublisherRun(t *testing.T) {
	config := validConfig()
	private := false
	assets := `[{"id":9,"name":"worker-release.json"}]`
	sourceSHA := strings.Repeat("a", 40)
	mainSHA := strings.Repeat("c", 40)
	sourceRootTree := strings.Repeat("1", 40)
	mainRootTree := strings.Repeat("2", 40)
	sourceDeployTree := strings.Repeat("3", 40)
	mainDeployTree := strings.Repeat("4", 40)
	sourceWorkerTree := strings.Repeat("5", 40)
	currentWorkerTree := sourceWorkerTree
	sourceGitHubTree := strings.Repeat("6", 40)
	mainGitHubTree := strings.Repeat("7", 40)
	sourceWorkflowsTree := strings.Repeat("8", 40)
	mainWorkflowsTree := strings.Repeat("9", 40)
	publisherWorkflowBlob := strings.Repeat("b", 40)
	buildInputIdentity := workerBuildInputIdentity(config, sourceWorkerTree, publisherWorkflowBlob)
	manifestData, err := json.Marshal(WorkerReleaseManifest{
		SchemaVersion:                2,
		WorkerVersion:                config.Worker.Version,
		SourceCommit:                 sourceSHA,
		Image:                        config.Worker.ImageRepository + "@sha256:" + strings.Repeat("b", 64),
		CodexVersion:                 config.Codex.Version,
		NoMistakesVersion:            config.NoMistakes.Version,
		NoMistakesUpstreamRepository: config.NoMistakes.UpstreamRepository,
		NoMistakesCommit:             config.NoMistakes.UpstreamCommit,
		NoMistakesForkRepository:     config.NoMistakes.ForkRepository,
		NoMistakesForkRelease:        config.NoMistakes.ForkRelease,
		NoMistakesLinuxAMD64SHA256:   config.NoMistakes.LinuxAMD64SHA256,
		BuildInputIdentity:           buildInputIdentity,
		GitHubActionsRunID:           123,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest := string(manifestData)
	configData, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	workflowID := int64(77)
	mergedBy := "skyhuang233"
	fullPullRequested := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/skyhuang233/workflow":
			_, _ = w.Write([]byte(fmt.Sprintf(`{"private":%t}`, private)))
		case "/repos/skyhuang233/workflow/releases/tags/" + workerReleaseTag(config.Worker.Version, buildInputIdentity):
			_, _ = w.Write([]byte(`{"target_commitish":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","assets":` + assets + `}`))
		case "/repos/skyhuang233/workflow/releases/assets/9":
			_, _ = w.Write([]byte(manifest))
		case "/repos/skyhuang233/workflow/commits/main":
			_, _ = w.Write([]byte(`{"sha":"` + mainSHA + `","commit":{"tree":{"sha":"` + mainRootTree + `"}}}`))
		case "/repos/skyhuang233/workflow/commits/" + sourceSHA:
			_, _ = w.Write([]byte(`{"sha":"` + sourceSHA + `","commit":{"tree":{"sha":"` + sourceRootTree + `"}}}`))
		case "/repos/skyhuang233/workflow/contents/config/toolchain.json":
			_, _ = w.Write(configData)
		case "/repos/skyhuang233/workflow/git/trees/" + sourceRootTree:
			_, _ = w.Write([]byte(`{"tree":[{"path":"deploy","type":"tree","sha":"` + sourceDeployTree + `"},{"path":".github","type":"tree","sha":"` + sourceGitHubTree + `"}]}`))
		case "/repos/skyhuang233/workflow/git/trees/" + mainRootTree:
			_, _ = w.Write([]byte(`{"tree":[{"path":"deploy","type":"tree","sha":"` + mainDeployTree + `"},{"path":".github","type":"tree","sha":"` + mainGitHubTree + `"}]}`))
		case "/repos/skyhuang233/workflow/git/trees/" + sourceDeployTree:
			_, _ = w.Write([]byte(`{"tree":[{"path":"worker","type":"tree","sha":"` + sourceWorkerTree + `"}]}`))
		case "/repos/skyhuang233/workflow/git/trees/" + mainDeployTree:
			_, _ = w.Write([]byte(`{"tree":[{"path":"worker","type":"tree","sha":"` + currentWorkerTree + `"}]}`))
		case "/repos/skyhuang233/workflow/git/trees/" + sourceGitHubTree:
			_, _ = w.Write([]byte(`{"tree":[{"path":"workflows","type":"tree","sha":"` + sourceWorkflowsTree + `"}]}`))
		case "/repos/skyhuang233/workflow/git/trees/" + mainGitHubTree:
			_, _ = w.Write([]byte(`{"tree":[{"path":"workflows","type":"tree","sha":"` + mainWorkflowsTree + `"}]}`))
		case "/repos/skyhuang233/workflow/git/trees/" + sourceWorkflowsTree, "/repos/skyhuang233/workflow/git/trees/" + mainWorkflowsTree:
			_, _ = w.Write([]byte(`{"tree":[{"path":"publish-worker.yml","type":"blob","sha":"` + publisherWorkflowBlob + `"}]}`))
		case "/repos/skyhuang233/workflow/actions/runs/123":
			_, _ = w.Write([]byte(`{"head_sha":"` + sourceSHA + `","head_branch":"main","event":"push","status":"completed","conclusion":"success","workflow_id":` + fmt.Sprint(workflowID) + `}`))
		case "/repos/skyhuang233/workflow/actions/workflows/publish-worker.yml":
			_, _ = w.Write([]byte(`{"id":77,"path":".github/workflows/publish-worker.yml","state":"active"}`))
		case "/repos/skyhuang233/workflow/commits/" + sourceSHA + "/pulls":
			_, _ = w.Write([]byte(`[{"number":17,"merged_at":"2026-08-01T00:00:00Z","merge_commit_sha":"` + sourceSHA + `","base":{"ref":"main"},"merged_by":null}]`))
		case "/repos/skyhuang233/workflow/pulls/17":
			fullPullRequested = true
			_, _ = w.Write([]byte(`{"merged_at":"2026-08-01T00:00:00Z","merge_commit_sha":"` + sourceSHA + `","base":{"ref":"main"},"merged_by":{"login":"` + mergedBy + `","type":"User"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	fetcher := ReleaseFetcher{APIBase: server.URL, HTTP: server.Client(), WorkflowRepository: config.Worker.ReleaseRepository}
	got, raw, err := fetcher.Fetch(context.Background(), config, "github_pat_test")
	if err != nil {
		t.Fatal(err)
	}
	if got.SourceCommit != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" || string(raw) != manifest {
		t.Fatalf("fetch = %#v, %s", got, raw)
	}
	if !fullPullRequested {
		t.Fatal("fetch did not load the full merged pull request")
	}
	assets = `[{"id":9,"name":"worker-release.json"},{"id":10,"name":"worker-release.json"}]`
	if _, _, err := fetcher.Fetch(context.Background(), config, "github_pat_test"); err == nil {
		t.Fatal("accepted a release with duplicate worker-release.json assets")
	}
	assets = `[{"id":9,"name":"worker-release.json"},{"id":10,"name":"unexpected-asset.txt"}]`
	if _, _, err := fetcher.Fetch(context.Background(), config, "github_pat_test"); err == nil {
		t.Fatal("accepted a release with an asset other than worker-release.json")
	}
	assets = `[{"id":9,"name":"worker-release.json"}]`
	workflowID = 88
	if _, _, err := fetcher.Fetch(context.Background(), config, "github_pat_test"); err == nil {
		t.Fatal("accepted a manifest attributed to an unrelated successful workflow")
	}
	workflowID = 77
	mergedBy = "workflow[bot]"
	if _, _, err := fetcher.Fetch(context.Background(), config, "github_pat_test"); err == nil {
		t.Fatal("accepted a bot-merged release")
	}
	mergedBy = "skyhuang233"
	private = true
	if _, _, err := fetcher.Fetch(context.Background(), config, "github_pat_test"); err == nil {
		t.Fatal("accepted a private Worker Release repository")
	}
	private = false
	currentWorkerTree = strings.Repeat("d", 40)
	if _, _, err := fetcher.Fetch(context.Background(), config, "github_pat_test"); err == nil {
		t.Fatal("accepted a manifest after the current Worker build context changed")
	}
}

func TestReleaseFetcherRejectsAnUnboundWorkflowRepository(t *testing.T) {
	config := validConfig()
	fetcher := ReleaseFetcher{WorkflowRepository: config.Worker.ReleaseRepository}
	config.Worker.ReleaseRepository = "other/workflow"
	if _, _, err := fetcher.Fetch(context.Background(), config, "github_pat_test"); err == nil || !strings.Contains(err.Error(), "must match") {
		t.Fatalf("accepted mismatched workflow repository: %v", err)
	}

	fetcher.WorkflowRepository = ""
	config.Worker.ReleaseRepository = validConfig().Worker.ReleaseRepository
	if _, _, err := fetcher.Fetch(context.Background(), config, "github_pat_test"); err == nil || !strings.Contains(err.Error(), "workflow repository") {
		t.Fatalf("accepted an absent workflow repository: %v", err)
	}
}
