package doctor

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestUnifiedPublisherAdmitsOnlyOwnerMergedVersionBranches(t *testing.T) {
	text := readWorkflow(t, ".github", "workflows", "publish-workflow.yml")
	for _, required := range []string{
		"branches: [main]",
		`gh api "repos/${GITHUB_REPOSITORY}/pulls/${pull_number}"`,
		`"release-${version}"|"hotfix-${version}"`,
		`.merged_by.login`,
		`.merged_by.type`,
		`endswith("[bot]")`,
		`.base.ref == "main"`,
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("unified publisher omits admission contract %q", required)
		}
	}
	for _, forbidden := range []string{"workflow_dispatch:", "publish-platform", "publish-worker"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("unified publisher retains legacy/manual entry %q", forbidden)
		}
	}
	for _, required := range []string{"0.0.0 is never publishable", "the first Workflow Release must be workflow-v0.0.1"} {
		if !strings.Contains(text, required) {
			t.Fatalf("unified publisher omits initial namespace guard %q", required)
		}
	}
}

func TestUnifiedPublisherTestsTheAcceptedMergeBeforeMutation(t *testing.T) {
	text := readWorkflow(t, ".github", "workflows", "publish-workflow.yml")
	acceptedMerge := strings.Index(text, "  accepted-merge:")
	worker := strings.Index(text, "  worker:")
	if acceptedMerge < 0 || worker <= acceptedMerge {
		t.Fatal("unified publisher omits the accepted-merge gate before the Worker build")
	}
	gate := text[acceptedMerge:worker]
	for _, required := range []string{"runs-on: windows-latest", "go test -p 1 ./...", "go vet ./..."} {
		if !strings.Contains(gate, required) {
			t.Fatalf("accepted-merge gate omits %q", required)
		}
	}
	workerBlock := text[worker:]
	if !strings.Contains(workerBlock, "needs: accepted-merge") {
		t.Fatal("Worker mutation does not wait for the accepted-merge gate")
	}
}

func TestUnifiedPublisherScansBeforePushAndPublishesExactlyThreeAssets(t *testing.T) {
	text := readWorkflow(t, ".github", "workflows", "publish-workflow.yml")
	scan := strings.Index(text, "name: Scan Worker before push")
	push := strings.Index(text, "name: Push only the scan-passing image")
	if scan < 0 || push <= scan {
		t.Fatal("Worker image is not scanned before its first push")
	}
	for _, required := range []string{
		"workflow-windows-amd64.zip", "workflow-release.json", "worker-sbom.spdx.json",
		"severity-cutoff: high", "only-fixed: true", "immutable == true",
		"build-${GITHUB_SHA}-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}",
		`([.assets[].name] | sort) == (["worker-sbom.spdx.json","workflow-release.json","workflow-windows-amd64.zip"] | sort)`,
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("unified publisher omits atomic release contract %q", required)
		}
	}
	if strings.Count(text, "for name in worker-sbom.spdx.json workflow-release.json workflow-windows-amd64.zip") != 2 ||
		strings.Count(text, `git/ref/tags/${tag}`) < 4 {
		t.Fatal("fresh and idempotent publication do not both verify asset digests and the direct tag ref")
	}
	for _, retired := range []string{"build_input_identity", "build-input.json", "no_mistakes_fork_release", "no_mistakes_upstream_commit"} {
		if strings.Contains(text, retired) {
			t.Fatalf("unified publisher retains retired provenance contract %q", retired)
		}
	}
}

func TestUnifiedPublisherResolvesRetryBeforeBuildingWorker(t *testing.T) {
	text := readWorkflow(t, ".github", "workflows", "publish-workflow.yml")
	preflight := strings.Index(text, "name: Preflight Workflow Release target")
	build := strings.Index(text, "name: Build release candidate locally")
	push := strings.Index(text, "name: Push only the scan-passing image")
	resolve := strings.Index(text, "name: Resolve fresh, retry, or immutable state")
	if preflight < 0 || build <= preflight || push <= build || resolve <= push {
		t.Fatal("unified publisher does not preflight release retry state before building and pushing the Worker")
	}
	deleteDraft := strings.Index(text, `gh release delete "$tag" --yes --cleanup-tag=false`)
	if deleteDraft < preflight || deleteDraft >= build {
		t.Fatal("unified publisher does not delete a same-source retry draft during preflight")
	}
	if strings.Contains(text[resolve:], `gh release delete "$tag"`) {
		t.Fatal("unified publisher can delete a release after pushing the Worker")
	}
	stage := strings.Index(text, "name: Stage exactly three assets")
	if stage <= resolve || !strings.Contains(text[preflight:build], `.object.type == "commit" and .object.sha == $sha`) ||
		!strings.Contains(text[resolve:stage], `.object.type == "commit" and .object.sha == $sha`) {
		t.Fatal("unified publisher does not validate the direct Git tag before either building or creating a release")
	}
	for _, required := range []string{
		`[ "$release_state" = "fresh" ] && [ "$tag_exists" = "true" ]`,
		`allow_existing_tag: ${{ steps.preflight.outputs.allow_existing_tag }}`,
		`test "${{ needs.worker.outputs.allow_existing_tag }}" = "true"`,
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("unified publisher does not reserve an existing tag exclusively for a verified retry: missing %q", required)
		}
	}
}

func TestCandidateWorkflowCoversDevelopAndMainDryRun(t *testing.T) {
	text := readWorkflow(t, ".github", "workflows", "worker-contract.yml")
	for _, required := range []string{
		"branches: [develop, main]", "go test -p 1 ./...", "go vet ./...",
		"release-dry-run:", `github.base_ref == 'main'`,
		"Assemble qualification candidate without publication", "scripts/assemble-workflow-release.ps1",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("candidate workflow omits %q", required)
		}
	}
}

func TestQualificationCandidateUsesAvailableWorkerDigestWithoutBlockingOnScan(t *testing.T) {
	text := readWorkflow(t, ".github", "workflows", "worker-contract.yml")
	for _, required := range []string{
		"packages: write",
		"Scan qualification Worker dependencies without blocking functional qualification",
		"fail-build: false",
		"image: ${{ steps.image.outputs.reference }}",
		`docker push "$candidate"`,
		`docker buildx imagetools inspect "$image"`,
		"qualification_image: ${{ steps.qualification-image.outputs.image }}",
		"-WorkerImage '${{ needs.worker-contract.outputs.qualification_image }}'",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("qualification candidate omits available Worker image contract %q", required)
		}
	}
	if strings.Contains(text, "$dryRunDigest") || strings.Contains(text, "GetBytes('dry-run')") {
		t.Fatal("qualification candidate retains a synthetic Worker image digest")
	}
	if strings.Contains(text, "workflow-worker:candidate-") {
		t.Fatal("qualification candidate retains the retired local Worker image tag")
	}
	if strings.Count(text, "${{ steps.image.outputs.reference }}") != 3 {
		t.Fatal("qualification candidate does not consistently consume the emitted Worker image reference")
	}
	publisher := readWorkflow(t, ".github", "workflows", "publish-workflow.yml")
	scan := strings.Index(publisher, "name: Scan Worker before push")
	push := strings.Index(publisher, "name: Push only the scan-passing image")
	if scan < 0 || push <= scan || !strings.Contains(publisher[scan:push], "fail-build: true") {
		t.Fatal("formal publisher no longer blocks image publication on its vulnerability scan")
	}
}

func TestCandidateAndPublisherUseSharedWorkflowReleaseAssembler(t *testing.T) {
	candidate := readWorkflow(t, ".github", "workflows", "worker-contract.yml")
	publisher := readWorkflow(t, ".github", "workflows", "publish-workflow.yml")
	for name, text := range map[string]string{"candidate": candidate, "publisher": publisher} {
		if strings.Count(text, "& scripts/assemble-workflow-release.ps1") != 1 {
			t.Fatalf("%s workflow does not use exactly one shared assembler", name)
		}
		if strings.Contains(text, "go run ./cmd/workflow-release assemble") {
			t.Fatalf("%s workflow retains an independent release assembler", name)
		}
	}
	assembler := readWorkflow(t, "scripts", "assemble-workflow-release.ps1")
	for _, required := range []string{
		"$SourceCommit", "$GitHubActionsRunID", "$WorkerImage", "$SBOMPath",
		"go run ./cmd/workflow-release assemble",
		"-workflow-version-exe $workflowVersionExecutable",
		"Workflow Release manifest Bundle digest differs from assembled asset",
		"Workflow Release manifest SBOM digest differs from assembled asset",
		"worker-sbom.spdx.json workflow-release.json workflow-windows-amd64.zip",
	} {
		if !strings.Contains(assembler, required) {
			t.Fatalf("shared assembler omits %q", required)
		}
	}
}

func TestLegacyPublisherFilesAreRemoved(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	root := filepath.Join(filepath.Dir(file), "..", "..", ".github", "workflows")
	for _, name := range []string{"publish-platform.yml", "publish-worker.yml"} {
		if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Fatalf("legacy publisher %s still exists", name)
		}
	}
}

func readWorkflow(t *testing.T, path ...string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	workflow, err := os.ReadFile(filepath.Join(append([]string{filepath.Dir(file), "..", ".."}, path...)...))
	if err != nil {
		t.Fatal(err)
	}
	return string(workflow)
}
