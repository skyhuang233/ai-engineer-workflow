package setup

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/skyhuang233/workflow/internal/setupcontract"
)

func TestPowerShellCanonicalPlatformPlanParsesAndAppliesThroughGo(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("PowerShell bootstrap is Windows-only")
	}
	powershell, err := exec.LookPath("pwsh")
	if err != nil {
		powershell, err = exec.LookPath("powershell.exe")
	}
	if err != nil {
		t.Skip("PowerShell is unavailable")
	}
	plan := testPlan(filepath.Join(t.TempDir(), "WorkflowHome"))
	raw, _ := json.MarshalIndent(plan, "", "  ")
	input := filepath.Join(t.TempDir(), "plan.input.json")
	canonicalPath := filepath.Join(t.TempDir(), "plan.canonical.json")
	if err := os.WriteFile(input, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	_, current, _, _ := runtime.Caller(0)
	script := filepath.Clean(filepath.Join(filepath.Dir(current), "..", "..", "skills", "setup-agent-workflow", "scripts", "convert-to-setup-canonical-json.ps1"))
	output, err := exec.Command(powershell, "-NoProfile", "-File", script, "-InputPath", input, "-OutputPath", canonicalPath).CombinedOutput()
	if err != nil {
		t.Fatalf("PowerShell canonicalizer: %v: %s", err, output)
	}
	canonical, err := os.ReadFile(canonicalPath)
	if err != nil {
		t.Fatal(err)
	}
	_, parsedCanonical, digest, err := setupcontract.ParsePlan(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if string(parsedCanonical) != string(canonical) || digest != strings.TrimSpace(string(output)) {
		t.Fatalf("PowerShell/Go authority mismatch: %s %s", output, canonical)
	}
	adapter := &fakeAdapter{states: map[string]setupcontract.EffectStatus{}}
	result, err := (&Engine{Adapter: adapter}).Apply(t.Context(), canonical, digest)
	if err != nil || result.Status != setupcontract.ExecutionSucceeded || len(adapter.applied) != len(plan.Effects) {
		t.Fatalf("PowerShell plan apply result=%#v err=%v applied=%v", result, err, adapter.applied)
	}
}
