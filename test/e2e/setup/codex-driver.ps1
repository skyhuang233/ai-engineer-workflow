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
$entrySkillSpec = Require-Environment "WORKFLOW_SETUP_E2E_ENTRY_SKILL_SPEC"
$platformVersion = Require-Environment "WORKFLOW_SETUP_E2E_PLATFORM_VERSION"
$runID = Require-Environment "WORKFLOW_SETUP_E2E_RUN_ID"
$patInputPath = [IO.Path]::GetFullPath((Require-Environment "WORKFLOW_SETUP_E2E_PAT_FILE"))
if (-not (Test-Path -LiteralPath $patInputPath -PathType Leaf)) { throw "WORKFLOW_SETUP_E2E_PAT_FILE is unavailable" }
$setupToken = [IO.File]::ReadAllText($patInputPath).Trim()
if ([string]::IsNullOrWhiteSpace($setupToken)) { throw "WORKFLOW_SETUP_E2E_PAT_FILE is empty" }
if (-not (Test-Path -LiteralPath $repositoryPath -PathType Container)) { throw "Scenario repository does not exist" }
$qualificationMode = $env:WORKFLOW_SETUP_QUALIFICATION -ceq "1"
if (-not $qualificationMode -and $entrySkillSpec -notmatch '#workflow-v[0-9A-Za-z._-]+$') { throw "WORKFLOW_SETUP_E2E_ENTRY_SKILL_SPEC must use the skills CLI #workflow-v Git ref syntax" }

$qualificationInstruction = ""
if ($qualificationMode) {
    $candidateDirectory = [IO.Path]::GetFullPath((Require-Environment "WORKFLOW_SETUP_CANDIDATE_DIRECTORY"))
    $candidateVersion = Require-Environment "WORKFLOW_SETUP_CANDIDATE_VERSION"
    $candidateSourceCommit = Require-Environment "WORKFLOW_SETUP_CANDIDATE_SOURCE_COMMIT"
    if (-not (Test-Path -LiteralPath $candidateDirectory -PathType Container)) { throw "WORKFLOW_SETUP_CANDIDATE_DIRECTORY is unavailable" }
    if ($candidateVersion -cne $platformVersion) { throw "Workflow qualification candidate version differs from the requested Platform version" }
    if ($candidateSourceCommit -cnotmatch '^[0-9a-f]{40}$') { throw "Workflow qualification candidate source commit is invalid" }
    $entrySkillSpec = [IO.Path]::GetFullPath($entrySkillSpec)
    if (-not (Test-Path -LiteralPath (Join-Path $entrySkillSpec ".git") -PathType Container)) { throw "WORKFLOW_SETUP_E2E_ENTRY_SKILL_SPEC must be a local qualification checkout" }
    $entryHead = [string](git -C $entrySkillSpec rev-parse HEAD)
    if ($LASTEXITCODE -ne 0 -or $entryHead.Trim() -cne $candidateSourceCommit) { throw "local qualification checkout HEAD differs from WORKFLOW_SETUP_CANDIDATE_SOURCE_COMMIT" }
    $entrySkillStatus = [string](git -C $entrySkillSpec status --porcelain=v1 -- skills/setup-agent-workflow)
    if ($LASTEXITCODE -ne 0 -or -not [string]::IsNullOrWhiteSpace($entrySkillStatus)) { throw "local qualification checkout has uncommitted Setup Skill changes" }
    $qualificationInstruction = @"
This is release-branch qualification with WORKFLOW_SETUP_QUALIFICATION=1. The Setup Skill was installed from the exact local qualification checkout whose git rev-parse HEAD equals WORKFLOW_SETUP_CANDIDATE_SOURCE_COMMIT. Immediately use the installed skill's explicit test-only candidate acquisition boundary with the exact WORKFLOW_SETUP_CANDIDATE_DIRECTORY, WORKFLOW_SETUP_CANDIDATE_VERSION, and WORKFLOW_SETUP_CANDIDATE_SOURCE_COMMIT values already present in the process environment. Do not query, download, or require a published GitHub Release. Do not fall back from the candidate path to published acquisition.
"@
}

$driverRoot = Split-Path -Parent $MyInvocation.MyCommand.Path
$schemaPath = Join-Path $driverRoot "driver-result.schema.json"
$agentResultPath = Join-Path ([IO.Path]::GetTempPath()) ("workflow-codex-result-" + $runID + "-" + $scenario + ".json")
$eventsPath = Join-Path ([IO.Path]::GetTempPath()) ("workflow-codex-events-" + $runID + "-" + $scenario + ".jsonl")
$installedSkill = Join-Path $env:USERPROFILE ".agents\skills\setup-agent-workflow"

function Get-DisposableRepositories {
    if ($scenario -eq "organization-rejects-classic-pat") { return @() }
    $prefix = "workflow-setup-e2e-"
    $priorGitHubToken = $env:GH_TOKEN
    try {
        $env:GH_TOKEN = $setupToken
        $raw = gh repo list $owner --limit 1000 --json nameWithOwner --jq ".[].nameWithOwner"
        if ($LASTEXITCODE -ne 0) { throw "cannot enumerate disposable repositories for cleanup fencing" }
    } finally {
        if ($null -eq $priorGitHubToken) { Remove-Item Env:GH_TOKEN -ErrorAction SilentlyContinue } else { $env:GH_TOKEN = $priorGitHubToken }
    }
    return @($raw | Where-Object { ([string]$_).StartsWith("$owner/$prefix") })
}

$before = @(Get-DisposableRepositories)
try {
    if (Test-Path -LiteralPath $installedSkill) { Remove-Item -LiteralPath $installedSkill -Recurse -Force }
    & npx --yes skills@latest add $entrySkillSpec --skill setup-agent-workflow --agent codex -g -y
    if ($LASTEXITCODE -ne 0 -or -not (Test-Path -LiteralPath (Join-Path $installedSkill "SKILL.md") -PathType Leaf)) { throw "README npx skill installation failed" }

    $prompt = @"
Manually invoke `$setup-agent-workflow for the repository at: $repositoryPath

This is the authorized, disposable setup qualification scenario "$scenario" under GitHub owner "$owner".
Select exact Workflow Release version "$platformVersion"; do not fall back to a different stable release.
$qualificationInstruction
Follow the installed skill exactly. If the directory is not a Git repository, answer yes to its Git initialization question and run exactly `git init -b main`; never use the machine's implicit default branch. For every plan_required response, inspect the complete projection and approve only the exact displayed digest, then continue applying and verifying it. Pipe exactly `$plan.onboarding_plan | ConvertTo-Json -Depth 100 -Compress` as the one JSON object on stdin for onboarding apply; do not join or reconstruct native output. Do not reuse an earlier approved digest after any plan command failure: regenerate, display, and approve the complete current Plan again. If onboarding apply returns incomplete with preceding effects satisfied and only repository-contract-pr required, treat it as the owner merge gate: return its exact Plan Digest and Pull Request in blocker with platform_ready=true and repository_admitted=false. Do not regenerate or reapply an Onboarding Plan before that Pull Request is merged. On a later invocation after the owner confirms the merge, generate and approve one new current Plan, then apply and verify it. When a classic PAT is required, never read, echo, print, place it in an argument, or copy it into an environment variable. Verify it by piping `Get-Content -LiteralPath `$env:WORKFLOW_SETUP_E2E_PAT_FILE -Raw` to the installed skill's documented `powershell.exe -NoProfile -NonInteractive -File` command for `verify-github-pat.ps1`; never invoke that script through PowerShell's call operator. Do not approve effects outside the scenario repository, isolated Workflow Home, current-user Codex skills/PATH, Docker Desktop dependency, and repositories named $owner/workflow-setup-e2e-*.

Positive scenarios clean-new-repository, unrelated-dirty-files, and second-same-owner must finish with both Platform Ready and Repository Admitted, except that an initial invocation must return at the documented owner merge gate and the resumed invocation must finish after the human merge. Negative scenarios must stop at the exact expected blocker without weakening or bypassing the contract. Preserve unrelated dirty files byte-for-byte.

Return only the JSON object required by the supplied output schema. Include every disposable repository created or discovered during this scenario in temporary_repositories. Use null blocker on success; otherwise provide the exact blocker.
"@
    $prompt | & codex exec --dangerously-bypass-approvals-and-sandbox --skip-git-repo-check --ephemeral --ignore-user-config --json --output-schema $schemaPath --output-last-message $agentResultPath -C $repositoryPath - 2>&1 | Set-Content -LiteralPath $eventsPath -Encoding utf8
    if ($LASTEXITCODE -ne 0 -or -not (Test-Path -LiteralPath $agentResultPath -PathType Leaf)) { throw "Codex setup interaction failed" }
    $events = Get-Content -LiteralPath $eventsPath -Raw
    if ($events.Contains($setupToken)) { throw "Codex event stream leaked the setup PAT" }
    $raw = Get-Content -LiteralPath $agentResultPath -Raw
    if ($raw.Contains($setupToken)) { throw "Codex result leaked the setup PAT" }
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
