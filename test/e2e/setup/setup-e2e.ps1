[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$GitHubOwner,
    [Parameter(Mandatory = $true)][string]$DriverScript,
    [Parameter(Mandatory = $true)][string]$EntrySkillSpec,
    [Parameter(Mandatory = $true)][string]$PlatformVersion,
    [ValidateSet("standard", "organization-policy")][string]$QualificationMode = "standard",
    [string]$DifferentOwnerRepository,
    [string]$ClassicPATRejectedRepository
)

$ErrorActionPreference = "Stop"
if ($env:WORKFLOW_SETUP_E2E -ne "1") { throw "Set WORKFLOW_SETUP_E2E=1 to authorize disposable setup qualification" }
if (-not $IsWindows -and $PSVersionTable.PSEdition -eq "Core") { throw "Workflow Setup qualification requires Windows" }
if ([string]::IsNullOrWhiteSpace($env:WORKFLOW_SETUP_E2E_PAT)) { throw "WORKFLOW_SETUP_E2E_PAT is required" }
if ([string]::IsNullOrWhiteSpace($env:WORKFLOW_SETUP_E2E_CLEANUP_TOKEN)) { throw "WORKFLOW_SETUP_E2E_CLEANUP_TOKEN with repository listing and deletion capability is required" }
if (-not (Test-Path -LiteralPath $DriverScript -PathType Leaf)) { throw "DriverScript does not exist" }
if ($EntrySkillSpec -notmatch '@(platform-v[0-9A-Za-z._-]+|[0-9a-fA-F]{40})$') { throw "EntrySkillSpec must pin an exact published release tag or commit" }
if ($QualificationMode -eq "standard" -and [string]::IsNullOrWhiteSpace($DifferentOwnerRepository)) { throw "DifferentOwnerRepository is required for the standard qualification" }
if ($QualificationMode -eq "organization-policy" -and [string]::IsNullOrWhiteSpace($ClassicPATRejectedRepository)) { throw "ClassicPATRejectedRepository is required for organization-policy qualification" }

$qualificationRoot = Join-Path ([IO.Path]::GetTempPath()) ("workflow-setup-e2e-" + [Guid]::NewGuid().ToString("N"))
$profileRoot = Join-Path $qualificationRoot "profile"
$workflowHome = Join-Path $qualificationRoot "workflow-home"
$evidenceRoot = Join-Path $qualificationRoot "evidence"
$githubConfig = Join-Path $qualificationRoot "gh"
$repositories = [Collections.Generic.List[string]]::new()
$cleanupErrors = [Collections.Generic.List[string]]::new()
$runID = [Guid]::NewGuid().ToString("N")
$cleanupToken = $env:WORKFLOW_SETUP_E2E_CLEANUP_TOKEN
$setupToken = $env:WORKFLOW_SETUP_E2E_PAT
$env:GH_TOKEN = $cleanupToken
$cleanupBaseline = @(gh repo list $GitHubOwner --limit 1000 --json nameWithOwner --jq ".[].nameWithOwner" | Where-Object { ([string]$_).StartsWith("$GitHubOwner/workflow-setup-e2e-") })
if ($LASTEXITCODE -ne 0) { throw "Cleanup credential cannot enumerate disposable repositories" }
$env:GH_TOKEN = $setupToken
# The deletion credential is harness-only and must never enter Codex or a Worker.
Remove-Item Env:WORKFLOW_SETUP_E2E_CLEANUP_TOKEN

New-Item -ItemType Directory -Force -Path $profileRoot,$workflowHome,$evidenceRoot,$githubConfig | Out-Null
$prior = @{
    USERPROFILE = $env:USERPROFILE; HOME = $env:HOME; CODEX_HOME = $env:CODEX_HOME
    WORKFLOW_HOME = $env:WORKFLOW_HOME; GH_CONFIG_DIR = $env:GH_CONFIG_DIR; GH_TOKEN = $env:GH_TOKEN
	WORKFLOW_SETUP_E2E_GITHUB_OWNER = $env:WORKFLOW_SETUP_E2E_GITHUB_OWNER
	WORKFLOW_SETUP_E2E_ENTRY_SKILL_SPEC = $env:WORKFLOW_SETUP_E2E_ENTRY_SKILL_SPEC
	WORKFLOW_SETUP_E2E_PLATFORM_VERSION = $env:WORKFLOW_SETUP_E2E_PLATFORM_VERSION
	WORKFLOW_SETUP_E2E_CLEANUP_TOKEN = $cleanupToken
}
$sourceCodexHome = $prior.CODEX_HOME
if ([string]::IsNullOrWhiteSpace($sourceCodexHome)) { $sourceCodexHome = Join-Path $prior.USERPROFILE ".codex" }
$sourceCodexAuth = Join-Path $sourceCodexHome "auth.json"
if (-not (Test-Path -LiteralPath $sourceCodexAuth -PathType Leaf)) { throw "Existing Codex ChatGPT auth.json is required" }

function Invoke-Scenario([string]$Name, [scriptblock]$Prepare) {
    $target = Join-Path $qualificationRoot ("workflow-setup-e2e-" + $runID + "-" + $Name)
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

function Initialize-PublishedFixture([string]$Target, [string]$Repository) {
    if ($Repository -notmatch '^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$') { throw "Fixture repository must be canonical owner/name" }
    $resolvedTarget = [IO.Path]::GetFullPath($Target)
    if (-not $resolvedTarget.StartsWith([IO.Path]::GetFullPath($qualificationRoot))) { throw "Fixture target escaped qualification root" }
    Remove-Item -LiteralPath $resolvedTarget -Recurse -Force
    git clone --quiet ("https://github.com/" + $Repository + ".git") $resolvedTarget
    if ($LASTEXITCODE -ne 0) { throw "Cannot clone fixture repository $Repository" }
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
	$env:WORKFLOW_SETUP_E2E_ENTRY_SKILL_SPEC = $EntrySkillSpec
	$env:WORKFLOW_SETUP_E2E_PLATFORM_VERSION = $PlatformVersion

    if ($QualificationMode -eq "standard") {
      if ($DifferentOwnerRepository.Split('/')[0] -ieq $GitHubOwner) { throw "DifferentOwnerRepository must have a different owner" }
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
      Invoke-Scenario "different-owner" { param($target); Initialize-PublishedFixture $target $DifferentOwnerRepository }
    } else {
      if ($ClassicPATRejectedRepository.Split('/')[0] -ine $GitHubOwner) { throw "ClassicPATRejectedRepository must belong to GitHubOwner so owner mismatch cannot mask organization policy" }
      Invoke-Scenario "organization-rejects-classic-pat" { param($target); Initialize-PublishedFixture $target $ClassicPATRejectedRepository }
    }
} finally {
	$scenarioToken = $env:GH_TOKEN
	$env:GH_TOKEN = $cleanupToken
    $cleanupAfter = @(gh repo list $GitHubOwner --limit 1000 --json nameWithOwner --jq ".[].nameWithOwner" | Where-Object { ([string]$_).StartsWith("$GitHubOwner/workflow-setup-e2e-") })
    if ($LASTEXITCODE -ne 0) { $cleanupErrors.Add("Cleanup credential cannot enumerate repositories after qualification") }
    foreach ($repository in @($cleanupAfter | Where-Object { $_ -notin $cleanupBaseline })) {
        if ($repository -notin $repositories) { $repositories.Add([string]$repository) }
    }
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
