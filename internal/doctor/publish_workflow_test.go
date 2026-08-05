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
	if !strings.Contains(string(workflow), "-- deploy/worker .github/workflows/publish-worker.yml") {
		t.Fatal("publisher workflow changes do not require a Worker release")
	}
	if !strings.Contains(string(workflow), "schema_version:2") || !strings.Contains(string(workflow), "@base64") {
		t.Fatal("publisher workflow does not use the canonical base64 Worker input encoding")
	}
	if !strings.Contains(string(workflow), "worker-v${{ steps.pins.outputs.worker_version }}-$identity") {
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
	if !strings.Contains(string(workflow), "NO_MISTAKES_UPSTREAM_COMMIT=${{ steps.pins.outputs.no_mistakes_commit }}") {
		t.Fatal("publisher workflow does not pass the complete no-mistakes commit to the Worker build")
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
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	workflow, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", "deploy", "github", "workflow-contract.yml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(workflow)
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
