package doctor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	githubapi "github.com/skyhuang233/workflow/internal/github"
	"github.com/skyhuang233/workflow/internal/workflowrelease"
)

func TestExactWorkflowAssetsRequiresTheAtomicSet(t *testing.T) {
	valid := []releaseAsset{
		{ID: 1, Name: workflowrelease.BundleAssetName, Digest: "sha256:" + strings.Repeat("a", 64)},
		{ID: 2, Name: workflowrelease.ManifestAssetName, Digest: "sha256:" + strings.Repeat("b", 64)},
		{ID: 3, Name: workflowrelease.SBOMAssetName, Digest: "sha256:" + strings.Repeat("c", 64)},
	}
	if _, err := exactWorkflowAssets(valid); err != nil {
		t.Fatalf("valid assets: %v", err)
	}
	for name, mutate := range map[string]func([]releaseAsset) []releaseAsset{
		"missing":   func(v []releaseAsset) []releaseAsset { return v[:2] },
		"duplicate": func(v []releaseAsset) []releaseAsset { v[2].Name = v[1].Name; return v },
		"extra":     func(v []releaseAsset) []releaseAsset { return append(v, releaseAsset{ID: 4, Name: "extra"}) },
	} {
		t.Run(name, func(t *testing.T) {
			input := append([]releaseAsset(nil), valid...)
			if _, err := exactWorkflowAssets(mutate(input)); err == nil {
				t.Fatal("accepted a non-atomic asset set")
			}
		})
	}
}

func TestWorkerSBOMRequiresNamedSPDX23(t *testing.T) {
	if err := validateWorkerSBOM([]byte(`{"spdxVersion":"SPDX-2.3","name":"workflow-worker"}`)); err != nil {
		t.Fatal(err)
	}
	if err := validateWorkerSBOM([]byte(`{"spdxVersion":"SPDX-2.2","name":"workflow-worker"}`)); err == nil {
		t.Fatal("accepted a non-SPDX-2.3 document")
	}
}

func TestResolveWorkerBuildInputUsesThePublishedWorkflowBlob(t *testing.T) {
	sha := func(ch string) string { return strings.Repeat(ch, 40) }
	commit, root := sha("a"), sha("b")
	objects := map[string]string{
		"deploy": sha("c"), "worker": sha("d"), "cmd": sha("e"), "digest": sha("1"),
		"internal": sha("2"), "source": sha("3"), "go.mod": sha("4"), "go.sum": sha("5"),
		".github": sha("6"), "workflows": sha("7"), "publisher": sha("8"),
	}
	toolchain := `{"schema_version":7,"codex":{"version":"0.147.0"},"github_cli":{"version":"2.97.0","linux_amd64_sha256":"` + strings.Repeat("a", 64) + `"},"go":{"version":"1.25.12","linux_amd64_sha256":"` + strings.Repeat("b", 64) + `"},"no_mistakes":{"version":"v1","upstream_repository":"owner/upstream","upstream_commit":"` + sha("9") + `","fork_repository":"owner/fork","fork_commit":"` + sha("a") + `","fork_release":"v1","linux_amd64_sha256":"` + strings.Repeat("c", 64) + `"},"worker":{"image_repository":"ghcr.io/skyhuang233/workflow-worker","release_repository":"skyhuang233/ai-engineer-workflow"},"runtime":{},"github":{},"upgrade":{}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/skyhuang233/ai-engineer-workflow/commits/" + commit:
			fmt.Fprintf(w, `{"sha":"%s","commit":{"tree":{"sha":"%s"}}}`, commit, root)
		case "/repos/skyhuang233/ai-engineer-workflow/contents/config/toolchain.json":
			fmt.Fprint(w, toolchain)
		case "/repos/skyhuang233/ai-engineer-workflow/git/trees/" + root:
			fmt.Fprintf(w, `{"tree":[{"path":"deploy","type":"tree","sha":"%s"},{"path":"cmd","type":"tree","sha":"%s"},{"path":"internal","type":"tree","sha":"%s"},{"path":"go.mod","type":"blob","sha":"%s"},{"path":"go.sum","type":"blob","sha":"%s"},{"path":".github","type":"tree","sha":"%s"}]}`, objects["deploy"], objects["cmd"], objects["internal"], objects["go.mod"], objects["go.sum"], objects[".github"])
		case "/repos/skyhuang233/ai-engineer-workflow/git/trees/" + objects["deploy"]:
			fmt.Fprintf(w, `{"tree":[{"path":"worker","type":"tree","sha":"%s"}]}`, objects["worker"])
		case "/repos/skyhuang233/ai-engineer-workflow/git/trees/" + objects["cmd"]:
			fmt.Fprintf(w, `{"tree":[{"path":"delivery-source-digest","type":"tree","sha":"%s"}]}`, objects["digest"])
		case "/repos/skyhuang233/ai-engineer-workflow/git/trees/" + objects["internal"]:
			fmt.Fprintf(w, `{"tree":[{"path":"deliverysource","type":"tree","sha":"%s"}]}`, objects["source"])
		case "/repos/skyhuang233/ai-engineer-workflow/git/trees/" + objects[".github"]:
			fmt.Fprintf(w, `{"tree":[{"path":"workflows","type":"tree","sha":"%s"}]}`, objects["workflows"])
		case "/repos/skyhuang233/ai-engineer-workflow/git/trees/" + objects["workflows"]:
			fmt.Fprintf(w, `{"tree":[{"path":"publish-workflow.yml","type":"blob","sha":"%s"}]}`, objects["publisher"])
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := githubapi.NewClient(server.URL, "token", server.Client())
	input, err := resolveWorkerBuildInput(context.Background(), client, "skyhuang233/ai-engineer-workflow", commit)
	if err != nil {
		t.Fatal(err)
	}
	if input.GitInputs.PublishWorkflowBlob != objects["publisher"] || input.GitInputs.DeployWorkerTree != objects["worker"] {
		t.Fatalf("resolved wrong build input: %#v", input.GitInputs)
	}
	if _, err := input.Identity(); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseFetcherRejectsAnUnboundRepositoryBeforeNetwork(t *testing.T) {
	config := validConfig()
	if _, _, err := (ReleaseFetcher{WorkflowRepository: "other/repo"}).Fetch(context.Background(), config, "token"); err == nil || !strings.Contains(err.Error(), "must match") {
		t.Fatalf("unexpected error: %v", err)
	}
}
