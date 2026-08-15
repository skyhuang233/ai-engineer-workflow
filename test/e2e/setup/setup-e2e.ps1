[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$GitHubOwner,
    [Parameter(Mandatory = $true)][string]$DriverScript,
    [Parameter(Mandatory = $true)][string]$SkillSource
)

$ErrorActionPreference = "Stop"
if ($env:WORKFLOW_SETUP_E2E -ne "1") { throw "Set WORKFLOW_SETUP_E2E=1 to authorize disposable setup qualification" }
if (-not $IsWindows -and $PSVersionTable.PSEdition -eq "Core") { throw "Workflow Setup qualification requires Windows" }
if ([string]::IsNullOrWhiteSpace($env:WORKFLOW_SETUP_E2E_PAT)) { throw "WORKFLOW_SETUP_E2E_PAT is required" }
if ([string]::IsNullOrWhiteSpace($env:WORKFLOW_SETUP_E2E_CLEANUP_TOKEN)) { throw "WORKFLOW_SETUP_E2E_CLEANUP_TOKEN with delete_repo scope is required" }
if (-not (Test-Path -LiteralPath $DriverScript -PathType Leaf)) { throw "DriverScript does not exist" }
if (-not (Test-Path -LiteralPath (Join-Path $SkillSource "SKILL.md") -PathType Leaf)) { throw "SkillSource is not a setup-agent-workflow bundle" }

$qualificationRoot = Join-Path ([IO.Path]::GetTempPath()) ("workflow-setup-e2e-" + [Guid]::NewGuid().ToString("N"))
$profileRoot = Join-Path $qualificationRoot "profile"
$workflowHome = Join-Path $qualificationRoot "workflow-home"
$evidenceRoot = Join-Path $qualificationRoot "evidence"
$githubConfig = Join-Path $qualificationRoot "gh"
$repositories = [Collections.Generic.List[string]]::new()
$cleanupErrors = [Collections.Generic.List[string]]::new()
$runID = [Guid]::NewGuid().ToString("N")
$cleanupToken = $env:WORKFLOW_SETUP_E2E_CLEANUP_TOKEN
# The deletion credential is harness-only and must never enter Codex or a Worker.
Remove-Item Env:WORKFLOW_SETUP_E2E_CLEANUP_TOKEN

New-Item -ItemType Directory -Force -Path $profileRoot,$workflowHome,$evidenceRoot,$githubConfig | Out-Null
$prior = @{
    USERPROFILE = $env:USERPROFILE; HOME = $env:HOME; CODEX_HOME = $env:CODEX_HOME
    WORKFLOW_HOME = $env:WORKFLOW_HOME; GH_CONFIG_DIR = $env:GH_CONFIG_DIR; GH_TOKEN = $env:GH_TOKEN
	WORKFLOW_SETUP_E2E_GITHUB_OWNER = $env:WORKFLOW_SETUP_E2E_GITHUB_OWNER
	WORKFLOW_SETUP_E2E_SKILL_SOURCE = $env:WORKFLOW_SETUP_E2E_SKILL_SOURCE
	WORKFLOW_SETUP_E2E_CLEANUP_TOKEN = $cleanupToken
}
$sourceCodexHome = $prior.CODEX_HOME
if ([string]::IsNullOrWhiteSpace($sourceCodexHome)) { $sourceCodexHome = Join-Path $prior.USERPROFILE ".codex" }
$sourceCodexAuth = Join-Path $sourceCodexHome "auth.json"
if (-not (Test-Path -LiteralPath $sourceCodexAuth -PathType Leaf)) { throw "Existing Codex ChatGPT auth.json is required" }

function Invoke-Scenario([string]$Name, [scriptblock]$Prepare) {
    $target = Join-Path $qualificationRoot $Name
    New-Item -ItemType Directory -Force -Path $target | Out-Null
    & $Prepare $target
    $resultPath = Join-Path $evidenceRoot ($Name + ".json")
    $env:WORKFLOW_SETUP_E2E_SCENARIO = $Name
    $env:WORKFLOW_SETUP_E2E_REPOSITORY_PATH = $target
    $env:WORKFLOW_SETUP_E2E_RESULT_PATH = $resultPath
    & $DriverScript
    if ($LASTEXITCODE -ne 0 -or -not (Test-Path -LiteralPath $resultPath -PathType Leaf)) { throw "Scenario '$Name' did not produce a result" }
    $raw = Get-Content -LiteralPath $resultPath -Raw
    if ($raw.Contains($env:WORKFLOW_SETUP_E2E_PAT)) { throw "Scenario '$Name' leaked the PAT into evidence" }
    $result = $raw | ConvertFrom-Json
    foreach ($repository in @($result.temporary_repositories)) {
        if (-not ([string]$repository).StartsWith("$GitHubOwner/workflow-setup-e2e-")) { throw "Driver returned an unsafe cleanup repository '$repository'" }
        $repositories.Add([string]$repository)
    }
    if ($Name -in @("clean-new-repository","unrelated-dirty-files","second-same-owner")) {
        if (-not $result.platform_ready -or -not $result.repository_admitted) { throw "Scenario '$Name' did not reach both readiness gates" }
    } elseif ([string]::IsNullOrWhiteSpace([string]$result.blocker)) {
        throw "Negative scenario '$Name' did not report an exact blocker"
    }
}

try {
    $env:USERPROFILE = $profileRoot; $env:HOME = $profileRoot
    $env:CODEX_HOME = Join-Path $profileRoot ".codex"
	New-Item -ItemType Directory -Force -Path $env:CODEX_HOME | Out-Null
	Copy-Item -LiteralPath $sourceCodexAuth -Destination (Join-Path $env:CODEX_HOME "auth.json")
    $env:WORKFLOW_HOME = $workflowHome; $env:GH_CONFIG_DIR = $githubConfig
    $env:GH_TOKEN = $env:WORKFLOW_SETUP_E2E_PAT
    $env:WORKFLOW_SETUP_E2E_RUN_ID = $runID
	$env:WORKFLOW_SETUP_E2E_GITHUB_OWNER = $GitHubOwner
	$env:WORKFLOW_SETUP_E2E_SKILL_SOURCE = [IO.Path]::GetFullPath($SkillSource)

    Invoke-Scenario "clean-new-repository" { param($target) }
    Invoke-Scenario "unrelated-dirty-files" {
        param($target)
        git -C $target init -b main | Out-Null
        [IO.File]::WriteAllText((Join-Path $target "README.md"), "baseline`n")
        git -C $target add README.md; git -C $target -c user.name=e2e -c user.email=e2e@localhost commit -m baseline | Out-Null
        [IO.File]::WriteAllText((Join-Path $target "unrelated.txt"), "preserve exactly`n")
    }
    Invoke-Scenario "managed-path-drift" {
        param($target)
        git -C $target init -b main | Out-Null
        New-Item -ItemType Directory -Force -Path (Join-Path $target ".workflow") | Out-Null
        [IO.File]::WriteAllText((Join-Path $target ".workflow\repository.json"), "drift")
    }
    Invoke-Scenario "non-github-origin" {
        param($target)
        git -C $target init -b main | Out-Null
        git -C $target remote add origin https://example.invalid/not-github.git
    }
    Invoke-Scenario "second-same-owner" { param($target) }
    Invoke-Scenario "different-owner" { param($target) }
    Invoke-Scenario "organization-rejects-classic-pat" { param($target) }
} finally {
	$scenarioToken = $env:GH_TOKEN
	$env:GH_TOKEN = $cleanupToken
    foreach ($repository in $repositories) {
        try { gh repo delete $repository --yes } catch { $cleanupErrors.Add($_.Exception.Message) }
    }
	$env:GH_TOKEN = $scenarioToken
    try {
        $containers = @(docker ps -aq --filter "label=workflow.setup_e2e=$runID")
        foreach ($container in $containers) { if ($container) { docker rm -f $container | Out-Null } }
    } catch { $cleanupErrors.Add($_.Exception.Message) }
    foreach ($name in $prior.Keys) {
		if ($null -eq $prior[$name]) { Remove-Item -Path ("Env:" + $name) -ErrorAction SilentlyContinue }
		else { Set-Item -Path ("Env:" + $name) -Value $prior[$name] }
	}
    $resolvedRoot = [IO.Path]::GetFullPath($qualificationRoot)
    $resolvedTemp = [IO.Path]::GetFullPath([IO.Path]::GetTempPath())
    if ($cleanupErrors.Count -eq 0 -and $resolvedRoot.StartsWith($resolvedTemp) -and [IO.Path]::GetFileName($resolvedRoot).StartsWith("workflow-setup-e2e-")) {
        Remove-Item -LiteralPath $resolvedRoot -Recurse -Force
    }
}
if ($cleanupErrors.Count -gt 0) { throw ("Qualification cleanup failed: " + ($cleanupErrors -join "; ")) }
