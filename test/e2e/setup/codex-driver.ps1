[CmdletBinding()]
param()

$ErrorActionPreference = "Stop"

function Require-Environment([string]$Name) {
    $value = [Environment]::GetEnvironmentVariable($Name)
    if ([string]::IsNullOrWhiteSpace($value)) { throw "$Name is required by the setup DriverScript contract" }
    return $value
}

if ($env:WORKFLOW_SETUP_E2E -ne "1") { throw "Set WORKFLOW_SETUP_E2E=1 before running the Codex setup driver" }
$scenario = Require-Environment "WORKFLOW_SETUP_E2E_SCENARIO"
$repositoryPath = [IO.Path]::GetFullPath((Require-Environment "WORKFLOW_SETUP_E2E_REPOSITORY_PATH"))
$resultPath = [IO.Path]::GetFullPath((Require-Environment "WORKFLOW_SETUP_E2E_RESULT_PATH"))
$owner = Require-Environment "WORKFLOW_SETUP_E2E_GITHUB_OWNER"
$skillSource = [IO.Path]::GetFullPath((Require-Environment "WORKFLOW_SETUP_E2E_SKILL_SOURCE"))
$runID = Require-Environment "WORKFLOW_SETUP_E2E_RUN_ID"
if ([string]::IsNullOrWhiteSpace($env:WORKFLOW_SETUP_E2E_PAT)) { throw "WORKFLOW_SETUP_E2E_PAT is required" }
if (-not (Test-Path -LiteralPath $repositoryPath -PathType Container)) { throw "Scenario repository does not exist" }
if (-not (Test-Path -LiteralPath (Join-Path $skillSource "SKILL.md") -PathType Leaf)) { throw "WORKFLOW_SETUP_E2E_SKILL_SOURCE is not a setup-agent-workflow bundle" }

$driverRoot = Split-Path -Parent $MyInvocation.MyCommand.Path
$schemaPath = Join-Path $driverRoot "driver-result.schema.json"
$agentResultPath = Join-Path ([IO.Path]::GetTempPath()) ("workflow-codex-result-" + $runID + "-" + $scenario + ".json")
$eventsPath = Join-Path ([IO.Path]::GetTempPath()) ("workflow-codex-events-" + $runID + "-" + $scenario + ".jsonl")
$installedSkill = Join-Path $env:CODEX_HOME "skills\setup-agent-workflow"

function Get-DisposableRepositories {
    $prefix = "workflow-setup-e2e-"
    $raw = gh repo list $owner --limit 1000 --json nameWithOwner --jq ".[].nameWithOwner"
    if ($LASTEXITCODE -ne 0) { throw "cannot enumerate disposable repositories for cleanup fencing" }
    return @($raw | Where-Object { ([string]$_).StartsWith("$owner/$prefix") })
}

$before = @(Get-DisposableRepositories)
try {
    if (Test-Path -LiteralPath $installedSkill) { Remove-Item -LiteralPath $installedSkill -Recurse -Force }
    New-Item -ItemType Directory -Force -Path (Split-Path -Parent $installedSkill) | Out-Null
    Copy-Item -LiteralPath $skillSource -Destination $installedSkill -Recurse

    $prompt = @"
Manually invoke `$setup-agent-workflow for the repository at: $repositoryPath

This is the authorized, disposable setup qualification scenario "$scenario" under GitHub owner "$owner".
Follow the installed skill exactly. If the directory is not a Git repository, answer yes to its Git initialization question. For every plan_required response, inspect the complete projection and approve only the exact displayed digest, then continue applying and verifying it. When a classic PAT is required, never read, echo, print, or place it in an argument; pipe WORKFLOW_SETUP_E2E_PAT directly to the documented verification/apply command from PowerShell. Do not approve effects outside the scenario repository, isolated Workflow Home, current-user Codex skills/PATH, Docker Desktop dependency, and repositories named $owner/workflow-setup-e2e-*.

Positive scenarios clean-new-repository, unrelated-dirty-files, and second-same-owner must finish with both Platform Ready and Repository Admitted. Negative scenarios must stop at the exact expected blocker without weakening or bypassing the contract. Preserve unrelated dirty files byte-for-byte.

Return only the JSON object required by the supplied output schema. Include every disposable repository created or discovered during this scenario in temporary_repositories. Use null blocker on success; otherwise provide the exact blocker.
"@
    $prompt | & codex exec --dangerously-bypass-approvals-and-sandbox --skip-git-repo-check --ephemeral --ignore-user-config --json --output-schema $schemaPath --output-last-message $agentResultPath -C $repositoryPath - 2>&1 | Set-Content -LiteralPath $eventsPath -Encoding utf8
    if ($LASTEXITCODE -ne 0 -or -not (Test-Path -LiteralPath $agentResultPath -PathType Leaf)) { throw "Codex setup interaction failed" }
    $events = Get-Content -LiteralPath $eventsPath -Raw
    if ($events.Contains($env:WORKFLOW_SETUP_E2E_PAT)) { throw "Codex event stream leaked WORKFLOW_SETUP_E2E_PAT" }
    $raw = Get-Content -LiteralPath $agentResultPath -Raw
    if ($raw.Contains($env:WORKFLOW_SETUP_E2E_PAT)) { throw "Codex result leaked WORKFLOW_SETUP_E2E_PAT" }
    $result = $raw | ConvertFrom-Json
    if ([string]$result.scenario -ne $scenario) { throw "Codex result scenario does not match DriverScript input" }
    $after = @(Get-DisposableRepositories)
    $created = @($after | Where-Object { $_ -notin $before })
    $reported = @($result.temporary_repositories) + $created | Sort-Object -Unique
    $result.temporary_repositories = @($reported)
    $result | ConvertTo-Json -Depth 5 -Compress | Set-Content -LiteralPath $resultPath -Encoding utf8
} catch {
    $after = @(Get-DisposableRepositories)
    $created = @($after | Where-Object { $_ -notin $before })
    @{
        scenario = $scenario
        platform_ready = $false
        repository_admitted = $false
        temporary_repositories = @($created)
        blocker = $_.Exception.Message
    } | ConvertTo-Json -Depth 5 -Compress | Set-Content -LiteralPath $resultPath -Encoding utf8
	$global:LASTEXITCODE = 0
} finally {
    Remove-Item -LiteralPath $agentResultPath,$eventsPath -Force -ErrorAction SilentlyContinue
}
