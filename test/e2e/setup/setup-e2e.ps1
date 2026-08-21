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
if ([string]::IsNullOrWhiteSpace($env:WORKFLOW_SETUP_E2E_OWNER_TOKEN)) { throw "WORKFLOW_SETUP_E2E_OWNER_TOKEN with owner merge capability is required" }
if (-not (Test-Path -LiteralPath $DriverScript -PathType Leaf)) { throw "DriverScript does not exist" }
$candidateQualification = $env:WORKFLOW_SETUP_QUALIFICATION -eq "1"
if ($candidateQualification) {
    # Bind the local qualification checkout with git rev-parse HEAD before any skill executes.
    $EntrySkillSpec = [IO.Path]::GetFullPath($EntrySkillSpec)
    $insideWorkTree = [string](git -C $EntrySkillSpec rev-parse --is-inside-work-tree)
    if ($LASTEXITCODE -ne 0 -or $insideWorkTree.Trim() -cne "true") { throw "EntrySkillSpec must be a local qualification checkout" }
    $entryHead = [string](git -C $EntrySkillSpec rev-parse HEAD)
    if ($LASTEXITCODE -ne 0 -or $entryHead.Trim() -cne $env:WORKFLOW_SETUP_CANDIDATE_SOURCE_COMMIT) { throw "local qualification checkout HEAD differs from the candidate source commit" }
} elseif ($EntrySkillSpec -notmatch '#workflow-v[0-9A-Za-z._-]+$') {
    throw "EntrySkillSpec must use the skills CLI #workflow-v Git ref syntax"
}
if ($QualificationMode -eq "standard" -and [string]::IsNullOrWhiteSpace($DifferentOwnerRepository)) { throw "DifferentOwnerRepository is required for the standard qualification" }
if ($QualificationMode -eq "organization-policy" -and [string]::IsNullOrWhiteSpace($ClassicPATRejectedRepository)) { throw "ClassicPATRejectedRepository is required for organization-policy qualification" }

$qualificationRoot = Join-Path ([IO.Path]::GetTempPath()) ("workflow-setup-e2e-" + [Guid]::NewGuid().ToString("N"))
$profileRoot = Join-Path $qualificationRoot "profile"
$workflowHome = Join-Path $qualificationRoot "workflow-home"
$evidenceRoot = Join-Path $qualificationRoot "evidence"
$githubConfig = Join-Path $qualificationRoot "gh"
$repositories = [Collections.Generic.List[string]]::new()
$cleanupErrors = [Collections.Generic.List[string]]::new()
$qualificationError = $null
$runID = [Guid]::NewGuid().ToString("N")
$cleanupToken = $env:WORKFLOW_SETUP_E2E_CLEANUP_TOKEN
$ownerToken = $env:WORKFLOW_SETUP_E2E_OWNER_TOKEN
$setupToken = $env:WORKFLOW_SETUP_E2E_PAT
if ($ownerToken -ceq $setupToken) { throw "Owner merge credential must be separate from the Setup credential" }
$patInputPath = Join-Path $qualificationRoot "setup-pat.stdin"
$prior = @{
    USERPROFILE = $env:USERPROFILE; HOME = $env:HOME; CODEX_HOME = $env:CODEX_HOME
    WORKFLOW_HOME = $env:WORKFLOW_HOME; GH_CONFIG_DIR = $env:GH_CONFIG_DIR; GH_TOKEN = $env:GH_TOKEN
	WORKFLOW_SETUP_E2E_PAT = $env:WORKFLOW_SETUP_E2E_PAT
	WORKFLOW_SETUP_E2E_PAT_FILE = $env:WORKFLOW_SETUP_E2E_PAT_FILE
	WORKFLOW_SETUP_E2E_GITHUB_OWNER = $env:WORKFLOW_SETUP_E2E_GITHUB_OWNER
	WORKFLOW_SETUP_E2E_ENTRY_SKILL_SPEC = $env:WORKFLOW_SETUP_E2E_ENTRY_SKILL_SPEC
	WORKFLOW_SETUP_E2E_PLATFORM_VERSION = $env:WORKFLOW_SETUP_E2E_PLATFORM_VERSION
	WORKFLOW_SETUP_E2E_CLEANUP_TOKEN = $cleanupToken
	WORKFLOW_SETUP_E2E_OWNER_TOKEN = $ownerToken
	WORKFLOW_SETUP_E2E_PHASE = $env:WORKFLOW_SETUP_E2E_PHASE
	WORKFLOW_SETUP_E2E_APPROVED_DIGEST = $env:WORKFLOW_SETUP_E2E_APPROVED_DIGEST
	WORKFLOW_SETUP_E2E_DELIVERY_PLAN = $env:WORKFLOW_SETUP_E2E_DELIVERY_PLAN
	WORKFLOW_SETUP_E2E_TICKET = $env:WORKFLOW_SETUP_E2E_TICKET
	WORKFLOW_SETUP_QUALIFICATION = $env:WORKFLOW_SETUP_QUALIFICATION
	WORKFLOW_SETUP_CANDIDATE_DIRECTORY = $env:WORKFLOW_SETUP_CANDIDATE_DIRECTORY
	WORKFLOW_SETUP_CANDIDATE_VERSION = $env:WORKFLOW_SETUP_CANDIDATE_VERSION
	WORKFLOW_SETUP_CANDIDATE_SOURCE_COMMIT = $env:WORKFLOW_SETUP_CANDIDATE_SOURCE_COMMIT
	WORKFLOW_SETUP_CANDIDATE_QUALIFICATION_RUN_ID = $env:WORKFLOW_SETUP_CANDIDATE_QUALIFICATION_RUN_ID
	WORKFLOW_SETUP_CANDIDATE_QUALIFICATION_RUN_ATTEMPT = $env:WORKFLOW_SETUP_CANDIDATE_QUALIFICATION_RUN_ATTEMPT
}
try {
$env:GH_TOKEN = $cleanupToken
$cleanupBaseline = @(gh repo list $GitHubOwner --limit 1000 --json nameWithOwner --jq ".[].nameWithOwner" | Where-Object { ([string]$_).StartsWith("$GitHubOwner/workflow-setup-e2e-") })
if ($LASTEXITCODE -ne 0) { throw "Cleanup credential cannot enumerate disposable repositories" }
$env:GH_TOKEN = $setupToken
# The deletion credential is harness-only and must never enter Codex or a Worker.
Remove-Item Env:WORKFLOW_SETUP_E2E_CLEANUP_TOKEN
Remove-Item Env:WORKFLOW_SETUP_E2E_OWNER_TOKEN

New-Item -ItemType Directory -Force -Path $profileRoot,$workflowHome,$evidenceRoot,$githubConfig | Out-Null
[IO.File]::WriteAllText($patInputPath, $setupToken, (New-Object Text.UTF8Encoding($false)))
$patACL = New-Object Security.AccessControl.FileSecurity
$patACL.SetAccessRuleProtection($true, $false)
$patACL.AddAccessRule((New-Object Security.AccessControl.FileSystemAccessRule([Security.Principal.WindowsIdentity]::GetCurrent().User, "FullControl", "Allow")))
Set-Acl -LiteralPath $patInputPath -AclObject $patACL
$env:WORKFLOW_SETUP_E2E_PAT_FILE = $patInputPath
Remove-Item Env:WORKFLOW_SETUP_E2E_PAT
$codexDoctor = (& codex doctor --json | ConvertFrom-Json)
$authCheck = $codexDoctor.checks.'auth.credentials'
$configCheck = $codexDoctor.checks.'config.load'
$sourceCodexAuth = [string]$authCheck.details.'auth file'
$doctorCodexHome = [string]$configCheck.details.CODEX_HOME
if ([int]$codexDoctor.schemaVersion -ne 1 -or [string]$authCheck.status -ne "ok" -or [string]$configCheck.status -ne "ok" -or [string]$authCheck.details.'stored ChatGPT tokens' -ne "true" -or [string]$authCheck.details.'stored auth mode' -ne "chatgpt") { throw "codex doctor --json did not verify a supported ChatGPT login" }
if (-not [IO.Path]::IsPathFullyQualified($sourceCodexAuth) -or -not [IO.Path]::IsPathFullyQualified($doctorCodexHome) -or -not [string]::Equals([IO.Path]::GetFullPath((Split-Path -Parent $sourceCodexAuth)), [IO.Path]::GetFullPath($doctorCodexHome), [StringComparison]::OrdinalIgnoreCase) -or -not (Test-Path -LiteralPath $sourceCodexAuth -PathType Leaf)) { throw "codex doctor --json returned an invalid authentication source boundary" }

function Invoke-DriverPhase([string]$Name, [string]$Target, [string]$Phase, [string]$ApprovedDigest = "", [long]$DeliveryPlan = 0, [long]$Ticket = 0) {
    $resultPath = Join-Path $evidenceRoot ($Name + "-" + $Phase + ".json")
    $env:WORKFLOW_SETUP_E2E_SCENARIO = $Name
    $env:WORKFLOW_SETUP_E2E_REPOSITORY_PATH = $Target
    $env:WORKFLOW_SETUP_E2E_RESULT_PATH = $resultPath
    $env:WORKFLOW_SETUP_E2E_PHASE = $Phase
    $env:WORKFLOW_SETUP_E2E_APPROVED_DIGEST = $ApprovedDigest
    $env:WORKFLOW_SETUP_E2E_DELIVERY_PLAN = $(if ($DeliveryPlan -gt 0) { [string]$DeliveryPlan } else { "" })
    $env:WORKFLOW_SETUP_E2E_TICKET = $(if ($Ticket -gt 0) { [string]$Ticket } else { "" })
    $driverGitHubToken = $env:GH_TOKEN
    Remove-Item Env:GH_TOKEN -ErrorAction SilentlyContinue
    try { & $DriverScript } finally { $env:GH_TOKEN = $driverGitHubToken }
    if ($LASTEXITCODE -ne 0 -or -not (Test-Path -LiteralPath $resultPath -PathType Leaf)) { throw "Scenario '$Name' phase '$Phase' did not produce a result" }
    $raw = Get-Content -LiteralPath $resultPath -Raw
    if ($raw.Contains($setupToken) -or $raw.Contains($ownerToken)) { throw "Scenario '$Name' leaked a credential into evidence" }
    $result = $raw | ConvertFrom-Json
    foreach ($repository in @($result.temporary_repositories)) {
        if (-not ([string]$repository).StartsWith("$GitHubOwner/workflow-setup-e2e-")) { throw "Driver returned an unsafe cleanup repository '$repository'" }
        $repositories.Add([string]$repository)
    }
    return $result
}

function Invoke-OwnerMerge($Gate, [string]$ExpectedGate) {
    if ([string]$Gate.gate_kind -cne $ExpectedGate -or [string]$Gate.pull_request -cnotmatch '^https://github\.com/[^/]+/[^/]+/pull/[1-9][0-9]*$' -or [string]$Gate.pull_head -cnotmatch '^[0-9a-f]{40}$' -or [string]$Gate.merge_method -cnotin @('merge','squash','rebase')) {
        throw "Owner merge gate lacks exact pull request identity"
    }
    if ($ExpectedGate -eq 'repository_onboarding' -and [string]$Gate.onboarding_plan_digest -cnotmatch '^[0-9a-f]{64}$') { throw "Onboarding owner gate lacks the approved Plan Digest" }
    $uri = [Uri]$Gate.pull_request
    $parts = $uri.AbsolutePath.Trim('/').Split('/')
    $repository = $parts[0] + '/' + $parts[1]
    $number = [long]$parts[3]
    if (-not $repository.StartsWith("$GitHubOwner/workflow-setup-e2e-")) { throw "Owner merge gate escaped the disposable repository boundary" }
    $savedToken = $env:GH_TOKEN
    try {
        $env:GH_TOKEN = $ownerToken
        $login = [string](gh api user --jq .login)
        if ($LASTEXITCODE -ne 0 -or -not [string]::Equals($login.Trim(), $GitHubOwner, [StringComparison]::OrdinalIgnoreCase)) { throw "Owner merge credential does not belong to GitHubOwner" }
        $pull = gh api "repos/$repository/pulls/$number" | ConvertFrom-Json
        if ($LASTEXITCODE -ne 0 -or [string]$pull.state -cne 'open' -or [string]$pull.head.sha -cne [string]$Gate.pull_head) { throw "Owner merge pull request changed before authorization" }
        if ($ExpectedGate -eq 'repository_onboarding' -and -not ([string]$pull.body).Contains([string]$Gate.onboarding_plan_digest)) { throw "Onboarding pull request does not bind the approved Plan Digest" }
        gh pr checks ([string]$Gate.pull_request) --required --watch --fail-fast --interval 10
        if ($LASTEXITCODE -ne 0) { throw "Owner merge pull request required checks have not passed" }
        $methodFlag = '--' + [string]$Gate.merge_method
        gh pr merge ([string]$Gate.pull_request) $methodFlag --match-head-commit ([string]$Gate.pull_head)
        if ($LASTEXITCODE -ne 0) { throw "Owner-authorized pull request merge failed" }
        $merged = gh api "repos/$repository/pulls/$number" | ConvertFrom-Json
        if ($LASTEXITCODE -ne 0 -or [string]::IsNullOrWhiteSpace([string]$merged.merged_at) -or -not [string]::Equals([string]$merged.merged_by.login, $GitHubOwner, [StringComparison]::OrdinalIgnoreCase)) { throw "Pull request was not merged by the repository owner" }
    } finally {
        if ($null -eq $savedToken) { Remove-Item Env:GH_TOKEN -ErrorAction SilentlyContinue } else { $env:GH_TOKEN = $savedToken }
    }
}

function Invoke-Scenario([string]$Name, [scriptblock]$Prepare) {
    $target = Join-Path $qualificationRoot ("workflow-setup-e2e-" + $runID + "-" + $Name)
    New-Item -ItemType Directory -Force -Path $target | Out-Null
    & $Prepare $target
    $result = Invoke-DriverPhase $Name $target 'initial'
    if ($Name -in @("clean-new-repository","unrelated-dirty-files","second-same-owner")) {
        if ($result.platform_ready -ne $true -or $result.repository_admitted -ne $false) { throw "Scenario '$Name' bypassed the repository owner merge gate" }
        Invoke-OwnerMerge $result 'repository_onboarding'
        $result = Invoke-DriverPhase $Name $target 'onboarding-resume' ([string]$result.onboarding_plan_digest)
        if ($Name -eq 'clean-new-repository') {
            if ($result.platform_ready -ne $true -or $result.repository_admitted -ne $true -or [long]$result.delivery_plan -le 0 -or [long]$result.ticket -le 0) { throw "Scenario '$Name' did not reach the Worker pull request owner gate" }
            Invoke-OwnerMerge $result 'worker_delivery'
            $result = Invoke-DriverPhase $Name $target 'delivery-resume' '' ([long]$result.delivery_plan) ([long]$result.ticket)
            if ($null -ne $result.gate_kind -or [string]$result.ticket_status -cne 'Delivered' -or [string]$result.plan_status -cne 'Completed') { throw "Scenario '$Name' did not reach Ticket Delivered and Plan Completed" }
        }
        if ($null -ne $result.gate_kind -or -not [string]::IsNullOrWhiteSpace([string]$result.blocker) -or -not $result.platform_ready -or -not $result.repository_admitted) { throw "Scenario '$Name' did not reach both readiness gates" }
		if ($Name -eq "unrelated-dirty-files") {
			$unrelatedPath = Join-Path $target "unrelated.txt"
			if (-not (Test-Path -LiteralPath $unrelatedPath -PathType Leaf) -or [IO.File]::ReadAllText($unrelatedPath) -cne "preserve exactly`n") { throw "Published dirty scenario did not preserve unrelated.txt byte-for-byte" }
			$previousOptionalLocks = $env:GIT_OPTIONAL_LOCKS
			try {
				$env:GIT_OPTIONAL_LOCKS = "0"
				$dirtyStatus = [string](git -C $target status --porcelain=v1 --untracked-files=all -- unrelated.txt)
			} finally {
				$env:GIT_OPTIONAL_LOCKS = $previousOptionalLocks
			}
			if ($LASTEXITCODE -ne 0 -or $dirtyStatus.Trim() -cne "?? unrelated.txt") { throw "Published dirty scenario no longer preserves unrelated.txt as unrelated dirty state" }
		}
    } elseif ([string]::IsNullOrWhiteSpace([string]$result.blocker)) {
        throw "Negative scenario '$Name' did not report an exact blocker"
    }
}

function Invoke-SetupCredentialLeakScan {
    $processEnvironmentEvidence = Join-Path $evidenceRoot "process-environment.txt"
    $dockerInspectEvidence = Join-Path $evidenceRoot "docker-inspect.json"
    $dockerContainerEvidence = Join-Path $evidenceRoot "docker-containers.json"
    $savedGitHubToken = $env:GH_TOKEN
    try {
        Remove-Item Env:GH_TOKEN -ErrorAction SilentlyContinue
		$workflowExecutable = Join-Path $workflowHome "bin\workflow.exe"
		if (-not (Test-Path -LiteralPath $workflowExecutable -PathType Leaf)) { throw "Installed Workflow CLI is unavailable for the credential scan safe point" }
		& $workflowExecutable stop --workflow-home $workflowHome --timeout 30s | Out-Null
		if ($LASTEXITCODE -ne 0) { throw "Cannot stop the Control Plane before checkpointing credential evidence" }
        @(Get-ChildItem Env: | Sort-Object Name | ForEach-Object { $_.Name + "=" + $_.Value }) | Set-Content -LiteralPath $processEnvironmentEvidence -Encoding utf8

        $workerContainers = @(
            @(docker container ls --all --quiet --filter "label=workflow.run_id")
            if ($LASTEXITCODE -ne 0) { throw "Cannot enumerate Workflow Worker containers for credential qualification" }
            @(docker container ls --all --quiet --filter "label=workflow.control_plane")
            if ($LASTEXITCODE -ne 0) { throw "Cannot enumerate Workflow Control Plane containers for credential qualification" }
            @(docker container ls --all --quiet --filter "label=com.skyhuang233.workflow.setup-probe=true")
            if ($LASTEXITCODE -ne 0) { throw "Cannot enumerate Workflow setup probe containers for credential qualification" }
        ) | Where-Object { -not [string]::IsNullOrWhiteSpace([string]$_) } | Sort-Object -Unique
        if ($workerContainers.Count -gt 0) {
            @(docker container inspect $workerContainers) | Set-Content -LiteralPath $dockerInspectEvidence -Encoding utf8
            if ($LASTEXITCODE -ne 0) { throw "Cannot inspect Workflow Worker containers for credential qualification" }
            @(docker container ls --all --no-trunc --format "{{json .}}" --filter "label=workflow.run_id") | Set-Content -LiteralPath $dockerContainerEvidence -Encoding utf8
            if ($LASTEXITCODE -ne 0) { throw "Cannot record Workflow Worker container evidence" }
        } else {
            [IO.File]::WriteAllText($dockerInspectEvidence, "[]`n")
            [IO.File]::WriteAllText($dockerContainerEvidence, "[]`n")
        }

        $setupToken | & go run (Join-Path $PSScriptRoot "leakscan") --workflow-home $workflowHome --evidence-root $evidenceRoot
        if ($LASTEXITCODE -ne 0) { throw "Setup credential leak scan rejected qualification evidence" }
    } finally {
        if ($null -eq $savedGitHubToken) { Remove-Item Env:GH_TOKEN -ErrorAction SilentlyContinue } else { $env:GH_TOKEN = $savedGitHubToken }
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

    $env:USERPROFILE = $profileRoot; $env:HOME = $profileRoot
    $env:CODEX_HOME = Join-Path $profileRoot ".codex"
	New-Item -ItemType Directory -Force -Path $env:CODEX_HOME | Out-Null
	Copy-Item -LiteralPath $sourceCodexAuth -Destination (Join-Path $env:CODEX_HOME "auth.json")
    $env:WORKFLOW_HOME = $workflowHome; $env:GH_CONFIG_DIR = $githubConfig
    $env:GH_TOKEN = $setupToken
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
		$publishedDirtyRepository = "$GitHubOwner/workflow-setup-e2e-$runID-published-dirty"
		gh repo create $publishedDirtyRepository --private --source $target --remote origin --push
		if ($LASTEXITCODE -ne 0) { throw "Cannot create the published dirty fixture repository" }
		$repositories.Add($publishedDirtyRepository)
		$origin = [string](git -C $target remote get-url origin)
		if ($LASTEXITCODE -ne 0 -or $origin -notmatch '^https://github.com/.+\.git$') { throw "Published dirty fixture lacks a real GitHub origin" }
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
    # Interrupted setup scenarios are intentionally excluded: this qualification
    # covers completed or cleanly blocked setup attempts, not cross-invocation recovery.
    Invoke-SetupCredentialLeakScan
} catch {
    $qualificationError = $_.Exception
} finally {
	$scenarioToken = $env:GH_TOKEN
	$env:GH_TOKEN = $cleanupToken
    $cleanupAfter = @()
    try {
        $cleanupAfter = @(gh repo list $GitHubOwner --limit 1000 --json nameWithOwner --jq ".[].nameWithOwner" | Where-Object { ([string]$_).StartsWith("$GitHubOwner/workflow-setup-e2e-") })
        $listExit = $LASTEXITCODE
        if ($listExit -ne 0) { $cleanupErrors.Add("Cleanup credential cannot enumerate repositories after qualification (exit $listExit)") }
    } catch { $cleanupErrors.Add($_.Exception.Message) }
    foreach ($repository in @($cleanupAfter | Where-Object { $_ -notin $cleanupBaseline })) {
        if ($repository -notin $repositories) { $repositories.Add([string]$repository) }
    }
    foreach ($repository in $repositories) {
        try {
            gh repo delete $repository --yes
            $deleteExit = $LASTEXITCODE
            if ($deleteExit -ne 0) { $cleanupErrors.Add("Cannot delete disposable repository $repository (exit $deleteExit)") }
        } catch { $cleanupErrors.Add($_.Exception.Message) }
    }
	$env:GH_TOKEN = $scenarioToken
    try {
        $containers = @(docker ps -aq --filter "label=workflow.setup_e2e=$runID")
        $dockerListExit = $LASTEXITCODE
        if ($dockerListExit -ne 0) { $cleanupErrors.Add("Cannot enumerate disposable containers (exit $dockerListExit)") }
        foreach ($container in $containers) {
            if ($container) {
                docker rm -f $container | Out-Null
                $dockerRemoveExit = $LASTEXITCODE
                if ($dockerRemoveExit -ne 0) { $cleanupErrors.Add("Cannot remove disposable container $container (exit $dockerRemoveExit)") }
            }
        }
    } catch { $cleanupErrors.Add($_.Exception.Message) }
    foreach ($name in $prior.Keys) {
		try {
            if ($null -eq $prior[$name]) { Remove-Item -Path ("Env:" + $name) -ErrorAction SilentlyContinue }
		    else { Set-Item -Path ("Env:" + $name) -Value $prior[$name] }
        } catch { $cleanupErrors.Add($_.Exception.Message) }
	}
    $resolvedRoot = [IO.Path]::GetFullPath($qualificationRoot)
    $resolvedTemp = [IO.Path]::GetFullPath([IO.Path]::GetTempPath())
    if ($resolvedRoot.StartsWith($resolvedTemp) -and [IO.Path]::GetFileName($resolvedRoot).StartsWith("workflow-setup-e2e-")) {
        try { Remove-Item -LiteralPath $resolvedRoot -Recurse -Force } catch { $cleanupErrors.Add($_.Exception.Message) }
    }
}
$failures = [Collections.Generic.List[string]]::new()
if ($null -ne $qualificationError) { $failures.Add("Qualification failed: " + $qualificationError.Message) }
foreach ($cleanupError in $cleanupErrors) { $failures.Add("Cleanup failed: " + $cleanupError) }
if ($failures.Count -gt 0) { throw ($failures -join "; ") }
