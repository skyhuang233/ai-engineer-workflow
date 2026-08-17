package doctor

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPublishWorkflowRequiresReleaseForPublisherChanges(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	workflow, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", ".github", "workflows", "publish-worker.yml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(workflow)
	for _, required := range []string{
		`- "deploy/worker/**"`,
		`- "cmd/delivery-source-digest/**"`,
		`- "internal/deliverysource/**"`,
		`- "go.mod"`,
		`- "go.sum"`,
		`- ".github/workflows/publish-worker.yml"`,
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("publisher workflow does not run for Worker identity input changes: missing %q", required)
		}
	}
	if !strings.Contains(text, "-- deploy/worker cmd/delivery-source-digest internal/deliverysource go.mod go.sum .github/workflows/publish-worker.yml") {
		t.Fatal("publisher workflow changes do not require a Worker release")
	}
	for _, required := range []string{
		"schema_version:6",
		"deploy_worker_tree:",
		"delivery_source_digest_command_tree:",
		"delivery_source_digest_package_tree:",
		"go_mod_blob:",
		"go_sum_blob:",
		"publish_worker_workflow_blob:",
		"@base64",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("publisher workflow does not use the canonical schema-6 Worker input encoding: missing %q", required)
		}
	}
	if !strings.Contains(text, "worker-v${{ steps.pins.outputs.worker_version }}-$identity") {
		t.Fatal("publisher workflow does not key Worker releases by build input identity")
	}
	if !strings.Contains(string(workflow), "[[ \"$identity\" =~ ^[0-9a-f]{64}$ ]]") {
		t.Fatal("publisher workflow does not validate the Worker identity with Bash syntax")
	}
	if !strings.Contains(string(workflow), "test \"$worker_release_repository\" = \"$GITHUB_REPOSITORY\"") ||
		!strings.Contains(string(workflow), "test \"$configured_owner\" = \"${GITHUB_REPOSITORY_OWNER,,}\"") {
		t.Fatal("publisher workflow does not enforce an owner-controlled same-repository release boundary")
	}
	if !strings.Contains(string(workflow), `gh api "repos/${GITHUB_REPOSITORY}"`) ||
		!strings.Contains(string(workflow), `(.full_name | ascii_downcase) == ($worker_release_repository | ascii_downcase)`) ||
		!strings.Contains(string(workflow), `(.owner.login | ascii_downcase) == $configured_owner`) {
		t.Fatal("publisher workflow does not admit GitHub canonical Worker Release repository identity")
	}
	if strings.Contains(string(workflow), ".private == false") || strings.Contains(string(workflow), "must be public") {
		t.Fatal("publisher workflow rejects an owner-controlled private release repository")
	}
	if !strings.Contains(string(workflow), "NO_MISTAKES_UPSTREAM_COMMIT=${{ steps.pins.outputs.no_mistakes_upstream_commit }}") ||
		!strings.Contains(string(workflow), "NO_MISTAKES_FORK_COMMIT=${{ steps.pins.outputs.no_mistakes_fork_commit }}") {
		t.Fatal("publisher workflow does not pass both no-mistakes provenance commits to the Worker build")
	}
	if !strings.Contains(string(workflow), "GO_LINUX_AMD64_SHA256=${{ steps.pins.outputs.go_sha256 }}") {
		t.Fatal("publisher workflow does not pass the pinned Go checksum to the Worker build")
	}
	if !strings.Contains(string(workflow), "GITHUB_CLI_LINUX_AMD64_SHA256=${{ steps.pins.outputs.github_cli_sha256 }}") {
		t.Fatal("publisher workflow does not pass the pinned GitHub CLI checksum to the Worker build")
	}
	for _, required := range []string{
		"draft: true",
		`gh release edit "$tag" --draft=false`,
		"(.immutable == true)",
	} {
		if !strings.Contains(string(workflow), required) {
			t.Fatalf("publisher workflow does not enforce immutable Worker releases: missing %q", required)
		}
	}
}

func TestPublishWorkflowLoadsFullPullBeforeOwnerAdmission(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	workflow, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", ".github", "workflows", "publish-worker.yml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(workflow)
	if !strings.Contains(text, `gh api "repos/${GITHUB_REPOSITORY}/pulls/${pull_number}"`) {
		t.Fatal("publisher workflow does not load the full pull request before checking merged_by")
	}
	if !strings.Contains(text, `test -n "$pull_number"`) {
		t.Fatal("publisher workflow does not fail closed when the merge commit lacks one unambiguous pull request")
	}
	if !strings.Contains(text, `((.merged_by.type? // "") | ascii_downcase) == "user"`) {
		t.Fatal("publisher workflow does not require a non-bot user from the full pull request")
	}
}

func TestIntegrationWorkflowAdmitsOwnerGuardedPrivateRepository(t *testing.T) {
	text := readWorkflow(t, "deploy", "github", "workflow-contract.yml")
	if strings.Contains(text, ".private == false") || strings.Contains(text, "public Owner-Guarded") {
		t.Fatal("integration workflow rejects an owner-controlled private repository")
	}
	if !strings.Contains(text, `CONFIGURED_INTEGRATION_REPOSITORY: ${{ vars.WORKFLOW_INTEGRATION_REPOSITORY }}`) ||
		!strings.Contains(text, `CONFIGURED_GATEWAY_CREDENTIAL_OWNER: ${{ vars.WORKFLOW_GATEWAY_CREDENTIAL_OWNER }}`) ||
		!strings.Contains(text, `test "$configured_repository" = "${GITHUB_REPOSITORY,,}"`) ||
		!strings.Contains(text, `(.full_name | ascii_downcase) == $configured_repository`) ||
		!strings.Contains(text, `(.owner.login | ascii_downcase) == $configured_owner`) ||
		!strings.Contains(text, `(.default_branch | length > 0)`) {
		t.Fatal("integration workflow does not verify its owner-controlled repository identity")
	}
}

func TestIntegrationWorkflowRunsWithoutControlPlaneSourceTree(t *testing.T) {
	text := readWorkflow(t, "deploy", "github", "workflow-contract.yml")
	if strings.Contains(text, "uses:") || strings.Count(text, "\n      - name:") != 1 ||
		!strings.Contains(text, "- name: Verify the Owner-Guarded repository contract") {
		t.Fatal("dedicated integration workflow contains steps outside its standalone GitHub contract")
	}
	for _, forbidden := range []string{"actions/checkout", "actions/setup-go", "go test ", "go run "} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("dedicated integration workflow depends on Control Plane source via %q", forbidden)
		}
	}
}

func TestIntegrationWorkflowSupportsPostVisibilityDispatch(t *testing.T) {
	text := readWorkflow(t, "deploy", "github", "workflow-contract.yml")
	if !strings.Contains(text, "workflow_dispatch:") {
		t.Fatal("dedicated integration workflow cannot be rerun after a visibility change")
	}
}

func TestWorkerContractRunsControlPlaneTestsForSourceChanges(t *testing.T) {
	text := readWorkflow(t, ".github", "workflows", "worker-contract.yml")
	for _, required := range []string{`- "**/*.go"`, `- "go.mod"`, `- "go.sum"`, "go test ./...", "go vet ./..."} {
		if !strings.Contains(text, required) {
			t.Fatalf("worker-contract does not preserve Control Plane test coverage: missing %q", required)
		}
	}
	controlPlaneStart := strings.Index(text, "\n  build-and-test:")
	workerContractStart := strings.Index(text, "\n  worker-contract:")
	if controlPlaneStart < 0 || workerContractStart <= controlPlaneStart {
		t.Fatal("worker-contract does not define separate Control Plane and Worker contract jobs")
	}
	controlPlaneJob := text[controlPlaneStart:workerContractStart]
	workerContractJob := text[workerContractStart:]
	for _, required := range []string{"runs-on: windows-latest", "TEMP: 'C:\\t'", "TMP: 'C:\\t'", "go test ./...", "go vet ./..."} {
		if !strings.Contains(controlPlaneJob, required) {
			t.Fatalf("Windows Control Plane job is missing %q", required)
		}
	}
	if !strings.Contains(workerContractJob, "runs-on: ubuntu-latest") {
		t.Fatal("Worker container contract job does not run on Linux")
	}
	for _, forbidden := range []string{"go test ./...", "go vet ./..."} {
		if strings.Contains(workerContractJob, forbidden) {
			t.Fatalf("Linux Worker contract job runs unsupported Control Plane command %q", forbidden)
		}
	}
	for _, required := range []string{"Detect Worker image input changes", "cmd/delivery-source-digest", "internal/deliverysource", "deploy/worker", "config/toolchain.json"} {
		if !strings.Contains(workerContractJob, required) {
			t.Fatalf("Worker container contract does not detect candidate image input %q", required)
		}
	}
	if got := strings.Count(workerContractJob, "if: steps.worker-changes.outputs.required == 'true'"); got != 5 {
		t.Fatalf("Worker container validation condition count = %d, want 5", got)
	}
}

func TestWorkerWorkflowsBindSBOMAndFailClosedOnFixableHighVulnerabilities(t *testing.T) {
	publish := readWorkflow(t, ".github", "workflows", "publish-worker.yml")
	contract := readWorkflow(t, ".github", "workflows", "worker-contract.yml")
	for name, workflow := range map[string]string{"publish-worker": publish, "worker-contract": contract} {
		for _, required := range []string{
			"anchore/sbom-action@e22c389904149dbc22b58101806040fa8d37a610",
			"anchore/scan-action@e1165082ffb1fe366ebaf02d8526e7c4989ea9d2",
			"output-file: worker-sbom.spdx.json",
			"sbom: worker-sbom.spdx.json",
			"fail-build: true",
			"severity-cutoff: high",
			"only-fixed: true",
		} {
			if !strings.Contains(workflow, required) {
				t.Fatalf("%s omits the Worker supply-chain gate %q", name, required)
			}
		}
	}
	for _, required := range []string{"sbom_sha256", "worker-sbom.spdx.json", "schema_version:6", "no_mistakes_upstream_commit", "no_mistakes_fork_commit"} {
		if !strings.Contains(publish, required) {
			t.Fatalf("publish-worker does not bind SBOM evidence in the release: missing %q", required)
		}
	}
}

func TestPlatformPublisherBindsGitHubHostedImmutableReleaseContractWithoutManagedKeys(t *testing.T) {
	workflow := readWorkflow(t, ".github", "workflows", "publish-platform.yml")
	for _, required := range []string{
		`- "internal/setupcontract/**"`,
		`- "internal/platformrelease/**"`,
		`- "deploy/platform/**"`,
		"go test ./internal/setupcontract ./internal/platformrelease ./internal/doctor",
		"GOOS: windows",
		"GOARCH: amd64",
		`GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}`,
		"config/platform-release.json",
		"workflow-windows-amd64.zip",
		"platform-release.json",
		"sha256sum --check SHA256SUMS",
		"--draft",
		`gh release edit "$tag" --draft=false`,
		"(.immutable == true)",
	} {
		if !strings.Contains(workflow, required) {
			t.Fatalf("Platform publisher omits release contract %q", required)
		}
	}
	for _, forbidden := range []string{
		"PLATFORM_RELEASE_SIGNING_KEY",
		"platform-release.json.sig",
		"-signing-key",
	} {
		if strings.Contains(workflow, forbidden) {
			t.Fatalf("Platform publisher still depends on a managed signing key or detached signature %q", forbidden)
		}
	}
	for _, forbidden := range []string{"platform-sbom.spdx.json", "platform-provenance.json", `commits/${GITHUB_SHA}/pulls`, "WORKFLOW_PLATFORM_VERSION", "DOCKER_DESKTOP_VERSION"} {
		if strings.Contains(workflow, forbidden) {
			t.Fatalf("Platform publisher retained removed release input or owner-PR check %q", forbidden)
		}
	}
	for _, required := range []string{"fetch-depth: 0", "workflow_dispatch:", "github.event.before", `github.event_name }}" = "workflow_dispatch"`, `git diff --quiet "$before" "$GITHUB_SHA"`, `git show "$before:config/platform-release.json"`} {
		if !strings.Contains(workflow, required) {
			t.Fatalf("Platform publisher does not compare Platform version across the complete push range: missing %q", required)
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
