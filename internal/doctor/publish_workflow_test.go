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
}
