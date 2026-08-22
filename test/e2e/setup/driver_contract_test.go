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
		`WORKFLOW_SETUP_E2E`, `WORKFLOW_SETUP_E2E_ENTRY_SKILL_SPEC`, `WORKFLOW_SETUP_E2E_PLATFORM_VERSION`, `WORKFLOW_SETUP_E2E_PAT_FILE`,
		`WORKFLOW_SETUP_QUALIFICATION`, `WORKFLOW_SETUP_CANDIDATE_DIRECTORY`, `WORKFLOW_SETUP_CANDIDATE_VERSION`, `WORKFLOW_SETUP_CANDIDATE_SOURCE_COMMIT`,
		`WORKFLOW_SETUP_CANDIDATE_QUALIFICATION_RUN_ID`, `WORKFLOW_SETUP_CANDIDATE_QUALIFICATION_RUN_ATTEMPT`, `WORKFLOW_SETUP_E2E_PHASE`,
		`local qualification checkout`, `git rev-parse HEAD`, `#workflow-v`,
		`Do not query, download, or require a published GitHub Release`,
		`git init -b main`, `Do not reuse an earlier approved digest after any plan command failure`,
		`powershell.exe -NoProfile -NonInteractive -File`, `verify-github-pat.ps1`,
		`repository-contract-pr`, `owner merge gate`, `Do not regenerate or reapply an Onboarding Plan before that Pull Request is merged`,
		`resume the exact stored Plan`, `with its preserved approved Digest and no stdin`,
		`Use pwsh for every other Setup script and Launcher command`, `Do not create helper scripts inside the target repository`,
		`Do not call gh repo create or push the repository directly`, `PAT verification must complete before any GitHub mutation`,
		`$setup-agent-workflow`, `codex exec`, `--output-schema`, `--output-last-message`,
		`--dangerously-bypass-approvals-and-sandbox`, `npx --yes skills@latest add`, `temporary_repositories`,
		`model_reasoning_effort="high"`, `qualification harness alone owns disposable-resource cleanup`,
		`Get-DisposableRepositories`, `result leaked the setup PAT`,
		`gate_kind="repository_onboarding"`, `gate_kind="worker_delivery"`, `Ticket #$ticket is projected as Delivered`, `Plan #$deliveryPlan is projected as Completed`,
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

func TestReadmePinsSetupSkillToImmutableWorkflowReleaseRef(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	readme := string(raw)
	if !strings.Contains(readme, "skyhuang233/ai-engineer-workflow#workflow-v0.0.1") {
		t.Fatal("README setup command is not pinned with the skills CLI Git ref syntax")
	}
	if strings.Contains(readme, "skills@latest add skyhuang233/ai-engineer-workflow --skill setup-agent-workflow") {
		t.Fatal("README setup command still acquires the default branch")
	}
}

func TestSetupSkillPinsFreshRepositoryMainAndExactOnboardingJSON(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "skills", "setup-agent-workflow", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	skill := string(raw)
	for _, required := range []string{
		"git init -b main",
		"Never initialize an unpublished repository with the machine's implicit default branch",
		"Do not reuse an earlier approved digest after any plan command failure",
		"one JSON object on stdin",
		"powershell.exe -NoProfile -NonInteractive -File",
		"verify-github-pat.ps1",
		"Do not pipe the PAT to verify-github-pat.ps1 through PowerShell's call operator",
		"repository-contract-pr",
		"Do not generate or apply another Onboarding Plan before the owner merges that exact Pull Request",
		"After the owner confirms the merge, do not generate a new Plan",
		"onboarding apply` with its preserved approved Digest and no stdin",
		"Use pwsh for every Setup script and Launcher command except the native powershell.exe PAT verification",
		"Never create a helper script inside the Onboarding target repository",
		"Never call gh repo create or push the Onboarding target directly",
		"PAT verification must succeed before any GitHub mutation",
	} {
		if !strings.Contains(skill, required) {
			t.Fatalf("Setup Skill lacks fresh-repository/Onboarding contract %q", required)
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
	for _, required := range []string{"scenario", "platform_ready", "repository_admitted", "temporary_repositories", "blocker", "gate_kind", "onboarding_plan_digest", "pull_request", "pull_head", "merge_method", "delivery_plan", "ticket", "ticket_status", "plan_status"} {
		if _, ok := schema.Properties[required]; !ok || !contains(schema.Required, required) {
			t.Fatalf("driver result schema lacks required property %q", required)
		}
	}
}

func TestDriverResultSchemaUsesCodexStructuredOutputsSubset(t *testing.T) {
	schema := read(t, "driver-result.schema.json")
	if strings.Contains(schema, `"uniqueItems"`) {
		t.Fatal("driver result schema uses unsupported Codex Structured Outputs keyword uniqueItems")
	}
}

func TestHarnessCopiesExistingCodexAuthIntoDisposableProfile(t *testing.T) {
	harness := read(t, "setup-e2e.ps1")
	for _, required := range []string{`codex doctor --json`, `stored ChatGPT tokens`, `stored auth mode`, `auth file`, "sourceCodexAuth", `Copy-Item -LiteralPath $sourceCodexAuth`, `WORKFLOW_SETUP_E2E_GITHUB_OWNER`, `WORKFLOW_SETUP_E2E_ENTRY_SKILL_SPEC`, `WORKFLOW_SETUP_E2E_PLATFORM_VERSION`, `WORKFLOW_SETUP_E2E_CLEANUP_TOKEN`, `Remove-Item Env:WORKFLOW_SETUP_E2E_CLEANUP_TOKEN`, `Wait-ForOwnerMerge`, `Wait-ForWorkflowContractCheck`, `check-runs?check_name=workflow-contract&filter=latest`, `[long]$_.app.id -eq 15368`, `Owner merge required`, `Assert-ControlPlaneCompletion`, `Assert-WorkflowContractProducer`, `<!-- workflow:status:start -->`, `delivery-resume`, `Initialize-PublishedFixture`, `DifferentOwnerRepository`, `ClassicPATRejectedRepository`, `#workflow-v`, `local qualification checkout`, `git rev-parse HEAD`, `rev-parse --is-inside-work-tree`} {
		if !strings.Contains(harness, required) {
			t.Fatalf("setup harness lacks %q", required)
		}
	}
	if strings.Contains(harness, "gh pr merge") || strings.Contains(harness, "WORKFLOW_SETUP_E2E_OWNER_TOKEN") || strings.Contains(harness, "gh pr checks") {
		t.Fatal("setup harness retains agent-reachable merge authority")
	}
	if runtime.GOOS != "windows" && strings.Contains(harness, "Windows qualification") {
		// The driver contract is intentionally Windows-only; static validation is
		// still portable and does not attempt to run Codex or mutate the host.
	}
}

func TestHarnessCapturesNativeCleanupExitCodesImmediately(t *testing.T) {
	lines := strings.Split(strings.ReplaceAll(read(t, "setup-e2e.ps1"), "\r\n", "\n"), "\n")
	for command, assignment := range map[string]string{
		"workflowExecutable stop": "$controlPlaneStopExit = $LASTEXITCODE",
		"gh repo list":            "$listExit = $LASTEXITCODE",
		"gh repo delete":          "$deleteExit = $LASTEXITCODE",
		"docker ps -aq":           "$dockerListExit = $LASTEXITCODE",
		"docker rm -f":            "$dockerRemoveExit = $LASTEXITCODE",
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

func TestHarnessRetriesTransientGitHubReadsWithinOwnerDeadline(t *testing.T) {
	harness := read(t, "setup-e2e.ps1")
	for _, required := range []string{
		"workflow-contract check; retrying within the owner merge timeout",
		"owner authorization; retrying within the owner merge timeout",
		"continue",
	} {
		if !strings.Contains(harness, required) {
			t.Fatalf("setup harness does not tolerate transient GitHub reads: missing %q", required)
		}
	}
}

func TestQualificationRepositoryDiscoveryIsScopedToCurrentRun(t *testing.T) {
	harness := read(t, "setup-e2e.ps1")
	driver := read(t, "codex-driver.ps1")
	for name, content := range map[string]string{"harness": harness, "driver": driver} {
		if !strings.Contains(content, `workflow-setup-e2e-$runID-`) {
			t.Fatalf("%s does not scope disposable repository discovery to the current qualification run", name)
		}
	}
	for _, required := range []string{
		`$runRepositoryPrefix = "$GitHubOwner/workflow-setup-e2e-$runID-"`,
		`StartsWith($runRepositoryPrefix, [StringComparison]::OrdinalIgnoreCase)`,
		`StartsWith($prefix, [StringComparison]::OrdinalIgnoreCase)`,
	} {
		if !strings.Contains(harness+"\n"+driver, required) {
			t.Fatalf("qualification repository fencing is missing %q", required)
		}
	}
}

func TestHarnessStopsControlPlaneBeforeRemovingQualificationRoot(t *testing.T) {
	harness := read(t, "setup-e2e.ps1")
	stop := strings.LastIndex(harness, `& $workflowExecutable stop --workflow-home $workflowHome --timeout 30s`)
	remove := strings.LastIndex(harness, "Remove-Item -LiteralPath $resolvedRoot")
	if stop < 0 || remove < 0 || stop > remove {
		t.Fatal("setup harness does not stop the Control Plane before removing qualification files")
	}
}

func TestHarnessComparesMergedProducerToQualifiedGitBlob(t *testing.T) {
	harness := read(t, "setup-e2e.ps1")
	for _, required := range []string{
		`$producerPath = 'deploy/platform/repository-contract/.github/workflows/workflow-contract.yml'`,
		`git -C $PSScriptRoot rev-parse "HEAD:$producerPath"`,
		`[string]$content.sha -cne $expectedBlob.Trim()`,
	} {
		if !strings.Contains(harness, required) {
			t.Fatalf("setup harness does not compare the merged producer to the qualified Git blob: missing %q", required)
		}
	}
	for _, checkoutByteComparison := range []string{"ReadAllBytes($source)", "$actualDigest", "$expectedDigest"} {
		if strings.Contains(harness, checkoutByteComparison) {
			t.Fatalf("setup harness compares checkout-transformed producer bytes via %q", checkoutByteComparison)
		}
	}
}

func TestHarnessPreservesQualificationFailureAndRunsEveryCleanup(t *testing.T) {
	harness := read(t, "setup-e2e.ps1")
	for _, required := range []string{"$qualificationError = $null", "$qualificationError = $_.Exception", "$failures.Add(\"Qualification failed:", "$failures.Add(\"Cleanup failed:", "$driverEvidence", "blocker='$([string]$result.blocker)'", "foreach ($repository in $repositories)", "docker ps -aq", "foreach ($name in $prior.Keys)", "Remove-Item -LiteralPath $resolvedRoot"} {
		if !strings.Contains(harness, required) {
			t.Fatalf("setup harness does not preserve primary failure while completing cleanup: missing %q", required)
		}
	}
	prior := strings.Index(harness, "$prior = @{")
	firstMutation := strings.Index(harness, "$env:GH_TOKEN = $cleanupToken")
	outerTry := strings.Index(harness, "try {")
	if prior < 0 || firstMutation < 0 || outerTry < 0 || prior > firstMutation || outerTry > firstMutation {
		t.Fatalf("outer environment was not captured before the full cleanup-protected mutation boundary")
	}
}

func TestHarnessScansSuccessfulSetupCredentialBoundariesWithOneDurableFingerprintAllowance(t *testing.T) {
	harness := read(t, "setup-e2e.ps1")
	scanner := read(t, "leakscan/main.go")
	qualification := harness + "\n" + scanner
	for _, required := range []string{
		"Invoke-SetupCredentialLeakScan", "WORKFLOW_SETUP_E2E_PAT", "fingerprint",
		"workflow-home", `filepath.WalkDir`, `filepath.Join(home, "state", "workflow.db")`, `[]string{home, evidence}`,
		"processEnvironmentEvidence", "dockerInspectEvidence", "dockerContainerEvidence",
		"github_pat_verifications.fingerprint_sha256", "authorization: bearer", "authorization: basic", "x-access-token:", "wal_checkpoint(TRUNCATE)", "VACUUM", "rawMainFingerprint",
	} {
		if !strings.Contains(qualification, required) {
			t.Fatalf("setup credential leak scan lacks required boundary/needle %q", required)
		}
	}
	if !strings.Contains(harness, `stop --workflow-home $workflowHome`) {
		t.Fatal("setup credential scan does not stop the Control Plane before SQLite compaction")
	}
	if !strings.Contains(harness, "Interrupted setup scenarios are intentionally excluded") {
		t.Fatal("setup credential leak scan does not record the intentional interrupted-setup exclusion")
	}
	for _, excluded := range []string{"interrupted-before-apply", "interrupted-during-apply", "interrupted-after-apply"} {
		if strings.Contains(harness, `Invoke-Scenario "`+excluded+`"`) {
			t.Fatalf("setup E2E unexpectedly claims unsupported interrupted scenario %q", excluded)
		}
	}
}

func TestUnrelatedDirtyQualificationUsesPublishedRepository(t *testing.T) {
	harness := read(t, "setup-e2e.ps1")
	start := strings.Index(harness, `Invoke-Scenario "unrelated-dirty-files"`)
	if start < 0 {
		t.Fatal("unrelated dirty scenario is missing")
	}
	end := strings.Index(harness[start:], `Invoke-Scenario "managed-path-drift"`)
	if end < 0 {
		t.Fatal("unrelated dirty scenario boundary is missing")
	}
	block := harness[start : start+end]
	create := strings.Index(block, "gh repo create")
	dirty := strings.Index(block, `unrelated.txt`)
	if create < 0 || dirty < 0 || create > dirty || !strings.Contains(block, "remote get-url origin") || !strings.Contains(block, "https://github.com/") {
		t.Fatalf("unrelated dirty scenario is not a real published-origin fixture:\n%s", block)
	}
	for _, assertion := range []string{"ReadAllText($unrelatedPath)", "preserve exactly", "status --porcelain=v1", "?? unrelated.txt"} {
		if !strings.Contains(harness, assertion) {
			t.Fatalf("published dirty postcondition lacks %q", assertion)
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
