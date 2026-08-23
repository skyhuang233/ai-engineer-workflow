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
		`.head.sha | select(test("^[0-9a-f]{40}$"))`,
		`(.parents | length) == 2`,
		`any(.parents[]; .sha == $head)`,
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
	candidate := strings.Index(text, "  qualified-candidate:")
	if acceptedMerge < 0 || candidate <= acceptedMerge {
		t.Fatal("unified publisher omits the accepted-merge gate before candidate resolution")
	}
	gate := text[acceptedMerge:candidate]
	for _, required := range []string{
		"runs-on: windows-latest",
		`TEMP: 'C:\t'`,
		`TMP: 'C:\t'`,
		"Prepare short Windows test paths",
		"New-Item -ItemType Directory -Force -Path $env:TEMP | Out-Null",
		"go test -p 1 ./...",
		"go vet ./...",
	} {
		if !strings.Contains(gate, required) {
			t.Fatalf("accepted-merge gate omits %q", required)
		}
	}
	candidateBlock := text[candidate:]
	if !strings.Contains(candidateBlock, "needs: accepted-merge") {
		t.Fatal("candidate resolution does not wait for the accepted-merge gate")
	}
}

func TestUnifiedPublisherConsumesExactRegistryPreservedQualification(t *testing.T) {
	text := readWorkflow(t, ".github", "workflows", "publish-workflow.yml")
	for _, required := range []string{
		"actions: read", "packages: read",
		`actions/workflows/worker-contract.yml/runs`,
		`-f event=pull_request -f head_sha="$head_sha" -f per_page=100`,
		`actions/runs/${candidate_run_id}/attempts/${attempt}`,
		`for ((attempt=latest_attempt; attempt>=1; attempt--))`,
		`.status == "completed" and .conclusion == "success"`,
		`.updated_at | fromdateiso8601`,
		`($merged | fromdateiso8601)`,
		`qualification_completed_at: ${{ steps.qualification.outputs.completed_at }}`,
		`merged_at: ${{ steps.admission.outputs.merged_at }}`,
		`candidate_tag="q-${head_sha}-${run_id}-${run_attempt}"`,
		`tagged_reference="${candidate_repository}:${candidate_tag}"`,
		`docker buildx imagetools inspect "$tagged_reference"`,
		`image="${candidate_repository}@${registry_digest}"`,
		`io.agent-workflow.candidate-source-commit`,
		`io.agent-workflow.qualification-run-id`,
		`io.agent-workflow.qualification-run-attempt`,
		`io.agent-workflow.manifest-sha256`,
		`qualified candidate manifest digest differs from registry identity`,
		`qualified candidate manifest differs from immutable registry identity`,
		`EXPECTED_SOURCE_COMMIT: ${{ steps.stored-candidate.outputs.expected_source }}`,
		`EXPECTED_QUALIFICATION_RUN_ID: ${{ steps.stored-candidate.outputs.expected_run_id }}`,
		`EXPECTED_QUALIFICATION_RUN_ATTEMPT: ${{ steps.stored-candidate.outputs.expected_run_attempt }}`,
		`-ExpectedSourceCommit $env:EXPECTED_SOURCE_COMMIT`,
		`-ExpectedQualificationRunID $env:EXPECTED_QUALIFICATION_RUN_ID`,
		`-ExpectedQualificationRunAttempt $env:EXPECTED_QUALIFICATION_RUN_ATTEMPT`,
		`-ExpectedSourceCommit '${{ needs.qualified-candidate.outputs.candidate_source_commit }}'`,
		`transferred qualification manifest digest differs`,
		`transferred qualification Worker digest differs`,
		"workflow-windows-amd64.zip", "workflow-release.json", "worker-sbom.spdx.json",
		"immutable == true",
		`([.assets[].name] | sort) == (["worker-sbom.spdx.json","workflow-release.json","workflow-windows-amd64.zip"] | sort)`,
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("unified publisher omits qualified-candidate contract %q", required)
		}
	}
	if strings.Contains(text, `.pull_requests[]?`) {
		t.Fatal("unified publisher relies on GitHub's transient workflow-run pull_requests projection after merge")
	}
	if strings.Contains(text, `-f status=completed`) || strings.Contains(text, `.workflow_runs[] | select(`+"\n"+`              .event == "pull_request" and .head_sha == $head and .conclusion == "success"`) {
		t.Fatal("unified publisher selects qualification from mutable latest-attempt state")
	}
	for _, forbidden := range []string{"actions/runs/${run_id}/artifacts", "qualified-workflow-release-", "packages/container/${candidate_package}/versions", "bash scripts/build-workflow-worker.sh", "docker push", "scripts/assemble-workflow-release.ps1", "anchore/scan-action"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("unified publisher can replace the qualified candidate through %q", forbidden)
		}
	}
	if strings.Count(text, "for name in worker-sbom.spdx.json workflow-release.json workflow-windows-amd64.zip") != 2 ||
		strings.Count(text, `git/ref/tags/${tag}`) < 4 {
		t.Fatal("fresh and idempotent publication do not both verify asset digests and the provenance tag ref")
	}
	for _, retired := range []string{"build_input_identity", "build-input.json", "no_mistakes_fork_release", "no_mistakes_upstream_commit"} {
		if strings.Contains(text, retired) {
			t.Fatalf("unified publisher retains retired provenance contract %q", retired)
		}
	}
}

func TestUnifiedPublisherVerifiesQualificationBeforeRetryMutation(t *testing.T) {
	text := readWorkflow(t, ".github", "workflows", "publish-workflow.yml")
	qualification := strings.Index(text, "name: Resolve exact successful qualification")
	preflight := strings.Index(text, "name: Preflight Workflow Release target")
	resolve := strings.Index(text, "name: Resolve fresh, retry, or immutable state")
	if qualification < 0 || preflight <= qualification || resolve <= preflight {
		t.Fatal("unified publisher does not verify qualification before mutating retry state")
	}
	deleteDraft := strings.Index(text, `gh release delete "$tag" --yes --cleanup-tag=false`)
	if deleteDraft < preflight || deleteDraft >= resolve {
		t.Fatal("unified publisher does not delete a same-source retry draft during preflight")
	}
	if strings.Contains(text[resolve:], `gh release delete "$tag"`) {
		t.Fatal("unified publisher can delete a release after acquiring the qualified candidate")
	}
	stage := strings.Index(text, "name: Stage exactly three assets")
	if stage <= resolve || !strings.Contains(text[preflight:resolve], `publisher tag lacks annotated provenance`) ||
		!strings.Contains(text[resolve:stage], `publisher tag lost annotated provenance`) {
		t.Fatal("unified publisher does not validate annotated tag provenance before either candidate acquisition or release creation")
	}
	for _, required := range []string{
		`tag_exists: ${{ steps.preflight.outputs.tag_exists }}`,
		`publisher_run_attempt: ${{ steps.preflight.outputs.publisher_run_attempt }}`,
		`.author.login == "github-actions[bot]" and .author.type == "Bot"`,
		`test "$(jq -r .run_id <<<"$provenance")" = "$GITHUB_RUN_ID"`,
		`gh release delete "$tag" --yes --cleanup-tag=false`,
		`gh release create "$tag"`,
		`--verify-tag`,
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
		"Restore approved workflow-v0.0.1 qualification candidate",
		"b35d239f4fe7e4ed55cb800942b2a36cf7468058",
		"q-$qualifiedSource-$qualifiedRunID-$qualifiedRunAttempt",
		"docker pull $reference",
		"docker create $reference",
		"docker cp \"${container}:/qualified/.\" build/release",
		"approved workflow-v0.0.1 candidate registry identity differs from its qualification evidence",
		"Qualify exact candidate setup and full delivery operation", "test/e2e/setup/setup-e2e.ps1",
		"if: github.head_ref != 'release-0.0.1'",
		"-GitHubOwner $env:WORKFLOW_SETUP_E2E_GITHUB_OWNER",
		"-DifferentOwnerRepository $env:WORKFLOW_SETUP_E2E_DIFFERENT_OWNER_REPOSITORY",
		"-OwnerMergeTimeoutMinutes 120",
		"Verify approved workflow-v0.0.1 functional qualification",
		"if: github.head_ref == 'release-0.0.1'",
		"$allowedQualificationOnlyChanges = @(",
		"'.github/workflows/publish-workflow.yml'",
		"'docs/adr/0002-accept-a-release-blackout-for-the-version-reset.md'",
		"$qualificationOnlyChanges.Count -ne $allowedQualificationOnlyChanges.Count",
		"workflow-v0.0.1 Bundle differs from the functionally qualified candidate",
		"workflow-v0.0.1 Worker differs from the functionally qualified candidate",
		"$qualifiedRunID = '32615246977'",
		"$qualifiedRunAttempt = '1'",
		"$env:GHCR_TOKEN | docker login ghcr.io",
		"$manifestSource = $head",
		"$manifestRunID = $runID",
		"$manifestRunAttempt = $attempt",
		"repos/skyhuang233/wf-use/pulls/1",
		"repos/skyhuang233/wf-use/pulls/5",
		"repos/skyhuang233/wf-use/issues/2/comments?per_page=100",
		"Delivered/Completed evidence is incomplete",
		"WORKFLOW_SETUP_CANDIDATE_QUALIFICATION_RUN_ATTEMPT", "workflow-release-qualification",
		`$maximumAttempts = 3`,
		`Start-Sleep -Seconds (5 * $attempt)`,
		"if: always()",
		`docker logout ghcr.io`,
		"Preserve exact qualified Workflow Release candidate",
		`$tag = "q-$head-$runID-$attempt"`,
		`$repository = "$([string]$toolchain.worker.image_repository)-release-candidate"`,
		`qualified candidate registry identity already exists`,
		`io.agent-workflow.candidate-source-commit`,
		`io.agent-workflow.qualification-run-id`,
		`io.agent-workflow.qualification-run-attempt`,
		`io.agent-workflow.manifest-sha256`,
		`docker push $reference`,
		`docker buildx imagetools inspect "$repository@$digest"`,
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("candidate workflow omits %q", required)
		}
	}
	for _, forbidden := range []string{
		"secrets.WORKFLOW_SETUP_E2E_PAT", "secrets.WORKFLOW_SETUP_E2E_CLEANUP_TOKEN",
		"vars.WORKFLOW_SETUP_E2E_GITHUB_OWNER", "vars.WORKFLOW_SETUP_E2E_DIFFERENT_OWNER_REPOSITORY",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("candidate workflow overwrites qualification-runner input with unconfigured GitHub value %q", forbidden)
		}
	}
}

func TestFirstReleasePublisherRetainsApprovedCandidateIdentity(t *testing.T) {
	text := readWorkflow(t, ".github", "workflows", "publish-workflow.yml")
	for _, required := range []string{
		`if [ "${{ steps.admission.outputs.version }}" = '0.0.1' ]; then`,
		"expected_source='b35d239f4fe7e4ed55cb800942b2a36cf7468058'",
		"expected_run_id='32615246977'",
		"expected_run_attempt='1'",
		"--arg source \"$expected_source\" --arg run \"$expected_run_id\" --arg attempt \"$expected_run_attempt\"",
		"-ExpectedSourceCommit $env:EXPECTED_SOURCE_COMMIT",
		"-ExpectedQualificationRunID $env:EXPECTED_QUALIFICATION_RUN_ID",
		"-ExpectedQualificationRunAttempt $env:EXPECTED_QUALIFICATION_RUN_ATTEMPT",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("first-release publisher omits approved candidate identity %q", required)
		}
	}
}

func TestQualificationCandidateUsesAvailableWorkerDigestWithoutBlockingOnScan(t *testing.T) {
	text := readWorkflow(t, ".github", "workflows", "worker-contract.yml")
	for _, required := range []string{
		"packages: write",
		"Scan qualification Worker dependencies without blocking functional qualification",
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
	qualificationScan := strings.Index(text, "name: Scan qualification Worker dependencies without blocking functional qualification")
	authenticate := strings.Index(text, "name: Authenticate qualification Worker registry")
	if qualificationScan < 0 || authenticate <= qualificationScan ||
		!strings.Contains(text[qualificationScan:authenticate], "continue-on-error: true") ||
		!strings.Contains(text[qualificationScan:authenticate], "fail-build: false") {
		t.Fatal("qualification scan can block functional candidate publication")
	}
	adr := readWorkflow(t, "docs", "adr", "0001-publish-one-atomic-workflow-release.md")
	for _, required := range []string{"Grype scan is", "advisory and non-blocking", "scanner failure do not prevent candidate preservation or publication"} {
		if !strings.Contains(adr, required) {
			t.Fatalf("release ADR omits advisory scan containment %q", required)
		}
	}
}

func TestOnlyQualificationAssemblesWorkflowRelease(t *testing.T) {
	candidate := readWorkflow(t, ".github", "workflows", "worker-contract.yml")
	publisher := readWorkflow(t, ".github", "workflows", "publish-workflow.yml")
	if strings.Count(candidate, "& scripts/assemble-workflow-release.ps1") != 1 {
		t.Fatal("qualification workflow does not assemble new release candidates exactly once")
	}
	if !strings.Contains(candidate, "Restore approved workflow-v0.0.1 qualification candidate") ||
		!strings.Contains(candidate, `docker cp "${container}:/qualified/." build/release`) {
		t.Fatal("first-release qualification does not restore the exact approved candidate")
	}
	if strings.Contains(candidate, "go run ./cmd/workflow-release assemble") {
		t.Fatal("qualification workflow retains an independent release assembler")
	}
	if strings.Contains(publisher, "assemble-workflow-release.ps1") || strings.Contains(publisher, "go run ./cmd/workflow-release assemble") {
		t.Fatal("publisher can assemble replacement release bytes")
	}
	assembler := readWorkflow(t, "scripts", "assemble-workflow-release.ps1")
	for _, required := range []string{
		"$CandidateSourceCommit", "$QualificationRunID", "$QualificationRunAttempt", "$WorkerImage", "$SBOMPath",
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

func TestOnlyQualificationBuildsWorkflowWorker(t *testing.T) {
	candidate := readWorkflow(t, ".github", "workflows", "worker-contract.yml")
	publisher := readWorkflow(t, ".github", "workflows", "publish-workflow.yml")
	if strings.Count(candidate, "bash scripts/build-workflow-worker.sh") != 1 {
		t.Fatal("qualification workflow does not use exactly one shared Worker builder")
	}
	if strings.Contains(candidate, "docker build \\") {
		t.Fatal("qualification workflow retains an independent Worker docker build")
	}
	if strings.Contains(publisher, "build-workflow-worker.sh") || strings.Contains(publisher, "docker build ") {
		t.Fatal("publisher can build a replacement Worker")
	}
	builder := readWorkflow(t, "scripts", "build-workflow-worker.sh")
	for _, required := range []string{
		"CODEX_VERSION", "GITHUB_CLI_VERSION", "GITHUB_CLI_LINUX_AMD64_SHA256",
		"GO_VERSION", "GO_LINUX_AMD64_SHA256", "NO_MISTAKES_VERSION",
		"NO_MISTAKES_REPOSITORY", "NO_MISTAKES_COMMIT",
		"deploy/worker/Dockerfile", "docker build",
	} {
		if !strings.Contains(builder, required) {
			t.Fatalf("shared Worker builder omits %q", required)
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
