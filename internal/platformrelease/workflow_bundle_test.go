package platformrelease

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWorkflowSkillBundleAutomaticallyBindsPublishedPlanRoot(t *testing.T) {
	_, current, _, _ := runtime.Caller(0)
	repository := filepath.Clean(filepath.Join(filepath.Dir(current), "..", ".."))
	bundle, err := os.ReadFile(filepath.Join(repository, "deploy", "platform", "skills", "agent-workflow", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(bundle)
	for _, required := range []string{"automatically", "workflow runtime-configure --source", "--root <plan-root-issue-number>", "not a user step", "codex doctor --json", "must not ask the user"} {
		if !strings.Contains(content, required) {
			t.Fatalf("Workflow Skill Bundle lacks automatic Plan Root binding contract %q", required)
		}
	}
	readme, err := os.ReadFile(filepath.Join(repository, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(readme), "workflow runtime-configure") {
		t.Fatal("README exposes internal runtime configuration as a user setup step")
	}
}
