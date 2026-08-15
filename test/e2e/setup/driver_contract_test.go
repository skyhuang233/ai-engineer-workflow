package setup_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRepositoryOwnedCodexDriverImplementsQualificationContract(t *testing.T) {
	driver := read(t, "codex-driver.ps1")
	for _, required := range []string{
		`WORKFLOW_SETUP_E2E`, `WORKFLOW_SETUP_E2E_ENTRY_SKILL_SPEC`, `WORKFLOW_SETUP_E2E_PLATFORM_VERSION`, `WORKFLOW_SETUP_E2E_PAT`,
		`$setup-agent-workflow`, `codex exec`, `--output-schema`, `--output-last-message`,
		`--dangerously-bypass-approvals-and-sandbox`, `npx --yes skills@latest add`, `temporary_repositories`,
		`Get-DisposableRepositories`, `result leaked WORKFLOW_SETUP_E2E_PAT`,
	} {
		if !strings.Contains(driver, required) {
			t.Fatalf("Codex DriverScript lacks required contract %q", required)
		}
	}
	for _, line := range strings.Split(driver, "\n") {
		if strings.Contains(line, "codex exec") && strings.Contains(line, "WORKFLOW_SETUP_E2E_PAT") {
			t.Fatal("Codex DriverScript places the PAT on the Codex command line")
		}
	}
}

func TestDriverResultSchemaIsStrictAndComplete(t *testing.T) {
	var schema struct {
		AdditionalProperties bool                      `json:"additionalProperties"`
		Required             []string                  `json:"required"`
		Properties           map[string]map[string]any `json:"properties"`
	}
	if err := json.Unmarshal([]byte(read(t, "driver-result.schema.json")), &schema); err != nil {
		t.Fatal(err)
	}
	if schema.AdditionalProperties {
		t.Fatal("driver result schema permits undeclared evidence")
	}
	for _, required := range []string{"scenario", "platform_ready", "repository_admitted", "temporary_repositories", "blocker"} {
		if _, ok := schema.Properties[required]; !ok || !contains(schema.Required, required) {
			t.Fatalf("driver result schema lacks required property %q", required)
		}
	}
}

func TestHarnessCopiesExistingCodexAuthIntoDisposableProfile(t *testing.T) {
	harness := read(t, "setup-e2e.ps1")
	for _, required := range []string{`codex doctor --json`, `stored ChatGPT tokens`, `stored auth mode`, `auth file`, "sourceCodexAuth", `Copy-Item -LiteralPath $sourceCodexAuth`, `WORKFLOW_SETUP_E2E_GITHUB_OWNER`, `WORKFLOW_SETUP_E2E_ENTRY_SKILL_SPEC`, `WORKFLOW_SETUP_E2E_PLATFORM_VERSION`, `WORKFLOW_SETUP_E2E_CLEANUP_TOKEN`, `Remove-Item Env:WORKFLOW_SETUP_E2E_CLEANUP_TOKEN`, `Initialize-PublishedFixture`, `DifferentOwnerRepository`, `ClassicPATRejectedRepository`} {
		if !strings.Contains(harness, required) {
			t.Fatalf("setup harness lacks %q", required)
		}
	}
	if runtime.GOOS != "windows" && strings.Contains(harness, "Windows qualification") {
		// The driver contract is intentionally Windows-only; static validation is
		// still portable and does not attempt to run Codex or mutate the host.
	}
}

func TestHarnessCapturesNativeCleanupExitCodesImmediately(t *testing.T) {
	lines := strings.Split(strings.ReplaceAll(read(t, "setup-e2e.ps1"), "\r\n", "\n"), "\n")
	for command, assignment := range map[string]string{
		"gh repo list":   "$listExit = $LASTEXITCODE",
		"gh repo delete": "$deleteExit = $LASTEXITCODE",
		"docker ps -aq":  "$dockerListExit = $LASTEXITCODE",
		"docker rm -f":   "$dockerRemoveExit = $LASTEXITCODE",
	} {
		found := false
		for index, line := range lines {
			if strings.Contains(line, command) && index+1 < len(lines) && strings.TrimSpace(lines[index+1]) == assignment {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s does not immediately preserve its native exit code in %s", command, assignment)
		}
	}
}

func TestHarnessPreservesQualificationFailureAndRunsEveryCleanup(t *testing.T) {
	harness := read(t, "setup-e2e.ps1")
	for _, required := range []string{"$qualificationError = $null", "$qualificationError = $_.Exception", "$failures.Add(\"Qualification failed:", "$failures.Add(\"Cleanup failed:", "foreach ($repository in $repositories)", "docker ps -aq", "foreach ($name in $prior.Keys)", "Remove-Item -LiteralPath $resolvedRoot"} {
		if !strings.Contains(harness, required) {
			t.Fatalf("setup harness does not preserve primary failure while completing cleanup: missing %q", required)
		}
	}
}

func TestPowerShellHarnessAndDriverParse(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		pwsh, err = exec.LookPath("powershell.exe")
	}
	if err != nil {
		t.Skip("PowerShell is unavailable")
	}
	for _, name := range []string{"setup-e2e.ps1", "codex-driver.ps1"} {
		absolute, err := filepath.Abs(name)
		if err != nil {
			t.Fatal(err)
		}
		quoted := strings.ReplaceAll(absolute, "'", "''")
		command := `$path='` + quoted + `'; $tokens=$null; $parseErrors=$null; [System.Management.Automation.Language.Parser]::ParseFile($path,[ref]$tokens,[ref]$parseErrors) | Out-Null; if ($parseErrors.Count -gt 0) { $parseErrors | ForEach-Object { Write-Error $_.Message }; exit 1 }`
		if output, err := exec.Command(pwsh, "-NoProfile", "-Command", command).CombinedOutput(); err != nil {
			t.Fatalf("%s does not parse: %v\n%s", name, err, output)
		}
	}
}

func read(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(".", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
