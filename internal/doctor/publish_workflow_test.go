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
}
