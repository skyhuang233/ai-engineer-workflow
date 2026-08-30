[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$GitHubOwner,
    [Parameter(Mandatory = $true)][string]$DriverScript,
    [Parameter(Mandatory = $true)][string]$EntrySkillSpec,
    [Parameter(Mandatory = $true)][string]$PlatformVersion,
    [ValidateSet("standard", "organization-policy")][string]$QualificationMode = "standard",
    [string]$DifferentOwnerRepository,
    [string]$ClassicPATRejectedRepository,
    [ValidateRange(1, 120)][int]$OwnerMergeTimeoutMinutes = 60
)

$ErrorActionPreference = "Stop"
if ($env:WORKFLOW_SETUP_E2E -ne "1") { throw "Set WORKFLOW_SETUP_E2E=1 to authorize disposable setup qualification" }
if (-not $IsWindows -and $PSVersionTable.PSEdition -eq "Core") { throw "Workflow Setup qualification requires Windows" }
if ([string]::IsNullOrWhiteSpace($env:WORKFLOW_SETUP_E2E_PAT)) { throw "WORKFLOW_SETUP_E2E_PAT is required" }
if ([string]::IsNullOrWhiteSpace($env:WORKFLOW_SETUP_E2E_CLEANUP_TOKEN)) { throw "WORKFLOW_SETUP_E2E_CLEANUP_TOKEN with repository listing and deletion capability is required" }
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
$runRepositoryID = $runID.Substring(0, 12)
$runRepositoryPrefix = "$GitHubOwner/wf-e2e-$runRepositoryID-"
$cleanupToken = $env:WORKFLOW_SETUP_E2E_CLEANUP_TOKEN
$setupToken = $env:WORKFLOW_SETUP_E2E_PAT
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
$cleanupBaseline = @(gh repo list $GitHubOwner --limit 1000 --json nameWithOwner --jq ".[].nameWithOwner" | Where-Object { ([string]$_).StartsWith($runRepositoryPrefix, [StringComparison]::OrdinalIgnoreCase) })
if ($LASTEXITCODE -ne 0) { throw "Cleanup credential cannot enumerate disposable repositories" }
$env:GH_TOKEN = $setupToken
# The deletion credential is harness-only and must never enter Codex or a Worker.
Remove-Item Env:WORKFLOW_SETUP_E2E_CLEANUP_TOKEN

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
    if ($raw.Contains($setupToken)) { throw "Scenario '$Name' leaked a credential into evidence" }
    $result = $raw | ConvertFrom-Json
    foreach ($repository in @($result.temporary_repositories)) {
        if (-not ([string]$repository).StartsWith($runRepositoryPrefix, [StringComparison]::OrdinalIgnoreCase)) { throw "Driver returned a cleanup repository outside this qualification run '$repository'" }
        if ($repository -notin $repositories) { $repositories.Add([string]$repository) }
    }
    return $result
}

function Wait-ForWorkflowContractCheck([string]$Repository, [string]$Head) {
    $deadline = [DateTime]::UtcNow.AddMinutes($OwnerMergeTimeoutMinutes)
    while ($true) {
        $checksRaw = gh api "repos/$Repository/commits/$Head/check-runs?check_name=workflow-contract&filter=latest&per_page=100"
        $checksExit = $LASTEXITCODE
        if ($checksExit -ne 0) {
            if ([DateTime]::UtcNow -ge $deadline) { throw "Timed out waiting for the exact GitHub Actions workflow-contract check after transient GitHub API failures" }
            Write-Warning "GitHub API read failed while awaiting the workflow-contract check; retrying within the owner merge timeout"
            Start-Sleep -Seconds 10
            continue
        }
        $checks = $checksRaw | ConvertFrom-Json
        $trusted = @($checks.check_runs | Where-Object {
            [string]$_.name -ceq 'workflow-contract' -and
            [string]$_.head_sha -ceq $Head -and
            [long]$_.app.id -eq 15368
        })
        if ($trusted.Count -gt 1) { throw "Owner merge head has ambiguous GitHub Actions workflow-contract checks" }
        if ($trusted.Count -eq 1) {
            if ([string]$trusted[0].status -ceq 'completed') {
                if ([string]$trusted[0].conclusion -cne 'success') { throw "Owner merge workflow-contract check did not succeed" }
                return
            }
        }
        if ([DateTime]::UtcNow -ge $deadline) { throw "Timed out waiting for the exact GitHub Actions workflow-contract check" }
        Start-Sleep -Seconds 10
    }
}

function Assert-WorkflowContractProducer([string]$Repository, [string]$Ref) {
    if ($Ref -cnotmatch '^[0-9a-f]{40}$') { throw "Merged onboarding workflow-contract producer ref is invalid" }
    $contentRaw = gh api "repos/$Repository/contents/.github/workflows/workflow-contract.yml?ref=$Ref"
    $contentExit = $LASTEXITCODE
    if ($contentExit -ne 0) { throw "Merged onboarding workflow-contract producer cannot be read" }
    $content = $contentRaw | ConvertFrom-Json
    if ([string]$content.path -cne '.github/workflows/workflow-contract.yml' -or [string]$content.encoding -cne 'base64') { throw "Merged onboarding workflow-contract producer identity is invalid" }
    $producerPath = 'deploy/platform/repository-contract/.github/workflows/workflow-contract.yml'
    $expectedBlob = [string](git -C $PSScriptRoot rev-parse "HEAD:$producerPath")
    $blobExit = $LASTEXITCODE
    if ($blobExit -ne 0 -or $expectedBlob.Trim() -cnotmatch '^[0-9a-f]{40}$') { throw "Qualified workflow-contract producer identity cannot be resolved" }
    if ([string]$content.sha -cne $expectedBlob.Trim()) { throw "Merged onboarding workflow-contract producer differs from the qualified release" }
}

function Wait-ForOwnerMerge($Gate, [string]$ExpectedGate) {
    if ([string]$Gate.gate_kind -cne $ExpectedGate -or [string]$Gate.pull_request -cnotmatch '^https://github\.com/[^/]+/[^/]+/pull/[1-9][0-9]*$' -or [string]$Gate.pull_head -cnotmatch '^[0-9a-f]{40}$' -or [string]$Gate.merge_method -cnotin @('merge','squash','rebase')) {
        throw "Owner merge gate lacks exact pull request identity"
    }
    if ($ExpectedGate -eq 'repository_onboarding' -and [string]$Gate.onboarding_plan_digest -cnotmatch '^[0-9a-f]{64}$') { throw "Onboarding owner gate lacks the approved Plan Digest" }
    $uri = [Uri]$Gate.pull_request
    $parts = $uri.AbsolutePath.Trim('/').Split('/')
    $repository = $parts[0] + '/' + $parts[1]
    $number = [long]$parts[3]
    if (-not $repository.StartsWith($runRepositoryPrefix, [StringComparison]::OrdinalIgnoreCase)) { throw "Owner merge gate escaped this qualification run's repository boundary" }
    $savedToken = $env:GH_TOKEN
    try {
        $env:GH_TOKEN = $setupToken
        $pullRaw = gh api "repos/$repository/pulls/$number"
        $pullExit = $LASTEXITCODE
        if ($pullExit -ne 0) { throw "Owner merge pull request cannot be read" }
        $pull = $pullRaw | ConvertFrom-Json
        if ([string]$pull.state -cne 'open' -or [string]$pull.head.sha -cne [string]$Gate.pull_head) { throw "Owner merge pull request changed before authorization" }
        if ($ExpectedGate -eq 'repository_onboarding' -and -not ([string]$pull.body).Contains([string]$Gate.onboarding_plan_digest)) { throw "Onboarding pull request does not bind the approved Plan Digest" }
        if ($ExpectedGate -eq 'worker_delivery') {
            Wait-ForWorkflowContractCheck $repository ([string]$Gate.pull_head)
            Write-Host "::notice title=Owner merge required::$($Gate.pull_request) passed the exact GitHub Actions workflow-contract check at head $($Gate.pull_head). The repository owner must authorize its $($Gate.merge_method) merge."
        } else {
            Write-Host "::notice title=Owner merge required::$($Gate.pull_request) is the exact approved onboarding pull request. The repository owner must authorize its $($Gate.merge_method) merge before setup verifies the installed workflow producer."
        }
        $deadline = [DateTime]::UtcNow.AddMinutes($OwnerMergeTimeoutMinutes)
        while ($true) {
            $pullRaw = gh api "repos/$repository/pulls/$number"
            $pullExit = $LASTEXITCODE
            if ($pullExit -ne 0) {
                if ([DateTime]::UtcNow -ge $deadline) { throw "Timed out waiting for the repository owner because the exact pull request could not be read" }
                Write-Warning "GitHub API read failed while awaiting owner authorization; retrying within the owner merge timeout"
                Start-Sleep -Seconds 15
                continue
            }
            $pull = $pullRaw | ConvertFrom-Json
            if ([string]$pull.head.sha -cne [string]$Gate.pull_head) { throw "Owner merge pull request head changed while awaiting authorization" }
            if (-not [string]::IsNullOrWhiteSpace([string]$pull.merged_at)) {
                if (-not [string]::Equals([string]$pull.merged_by.login, $GitHubOwner, [StringComparison]::OrdinalIgnoreCase)) { throw "Pull request was not merged by the repository owner" }
                break
            }
            if ([string]$pull.state -cne 'open') { throw "Owner merge pull request closed without the required merge" }
            if ([DateTime]::UtcNow -ge $deadline) { throw "Timed out waiting for the repository owner to authorize the exact pull request merge" }
            Start-Sleep -Seconds 15
        }
        if ($ExpectedGate -eq 'repository_onboarding') {
            Assert-WorkflowContractProducer $repository ([string]$pull.merge_commit_sha)
        }
    } finally {
        if ($null -eq $savedToken) { Remove-Item Env:GH_TOKEN -ErrorAction SilentlyContinue } else { $env:GH_TOKEN = $savedToken }
    }
}

function Assert-ControlPlaneCompletion([string]$Target, [long]$DeliveryPlan, [long]$Ticket) {
    $origin = [string](git -C $Target remote get-url origin)
    $originExit = $LASTEXITCODE
    if ($originExit -ne 0 -or $origin.Trim() -cnotmatch '^https://github\.com/(?<repository>[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+?)(?:\.git)?$') { throw "Completed delivery repository lacks a canonical GitHub origin" }
    $repository = $Matches.repository
    if ($repository.EndsWith('.git', [StringComparison]::OrdinalIgnoreCase)) { $repository = $repository.Substring(0, $repository.Length - 4) }
    if (-not $repository.StartsWith($runRepositoryPrefix, [StringComparison]::OrdinalIgnoreCase)) { throw "Completed delivery escaped this qualification run's repository boundary" }
    $savedToken = $env:GH_TOKEN
    try {
        $env:GH_TOKEN = $setupToken
        $planRaw = gh api "repos/$repository/issues/$DeliveryPlan"
        $planExit = $LASTEXITCODE
        if ($planExit -ne 0) { throw "Cannot read the exact Delivery Plan" }
        $planIssue = $planRaw | ConvertFrom-Json
        $ticketRaw = gh api "repos/$repository/issues/$Ticket"
        $ticketExit = $LASTEXITCODE
        if ($ticketExit -ne 0) { throw "Cannot read the exact Ticket" }
        $ticketIssue = $ticketRaw | ConvertFrom-Json
        if ([long]$planIssue.number -ne $DeliveryPlan -or [long]$ticketIssue.number -ne $Ticket -or $null -ne $planIssue.pull_request -or $null -ne $ticketIssue.pull_request) { throw "Delivery completion evidence does not identify the exact Plan and Ticket issues" }
        $comments = [Collections.Generic.List[object]]::new()
        for ($page = 1; ; $page++) {
            $commentsRaw = gh api "repos/$repository/issues/$DeliveryPlan/comments?per_page=100&page=$page"
            $commentsExit = $LASTEXITCODE
            if ($commentsExit -ne 0) { throw "Cannot read the exact Delivery Plan projection comments" }
            $commentPage = @($commentsRaw | ConvertFrom-Json)
            foreach ($comment in $commentPage) { $comments.Add($comment) }
            if ($commentPage.Count -lt 100) { break }
        }
        $controlPlaneMarker = '<!-- workflow:control-plane -->'
        $projectionComments = @($comments | Where-Object {
            [string]::Equals([string]$_.user.login, $GitHubOwner, [StringComparison]::OrdinalIgnoreCase) -and
            [string]$_.user.type -cne 'Bot' -and
            -not ([string]$_.user.login).EndsWith('[bot]', [StringComparison]::OrdinalIgnoreCase) -and
            ([string]$_.body).Contains($controlPlaneMarker)
        })
        if ($projectionComments.Count -ne 1) { throw "Delivery Plan lacks one owner-authored Control Plane projection comment" }
        $body = [string]$projectionComments[0].body
        $startMarker = '<!-- workflow:status:start -->'
        $endMarker = '<!-- workflow:status:end -->'
        $starts = [regex]::Matches($body, [regex]::Escape($startMarker))
        $ends = [regex]::Matches($body, [regex]::Escape($endMarker))
        if ($starts.Count -ne 1 -or $ends.Count -ne 1 -or $ends[0].Index -le $starts[0].Index) { throw "Delivery Plan lacks one authoritative Control Plane projection" }
        $projection = $body.Substring($starts[0].Index, $ends[0].Index + $endMarker.Length - $starts[0].Index)
        if (-not $projection.Contains('- state: `Completed`')) { throw "Control Plane projection does not report Plan Completed" }
        $ticketPattern = '(?m)^\|\s*#' + $Ticket + '\s+.*?\|\s*Delivered\s*\|'
        if ($projection -cnotmatch $ticketPattern) { throw "Control Plane projection does not report the exact Ticket Delivered" }
    } finally {
        if ($null -eq $savedToken) { Remove-Item Env:GH_TOKEN -ErrorAction SilentlyContinue } else { $env:GH_TOKEN = $savedToken }
    }
}

function Invoke-Scenario([string]$Name, [scriptblock]$Prepare) {
    $repositorySuffix = switch ($Name) {
        "clean-new-repository" { "clean" }
        "unrelated-dirty-files" { "dirty" }
        "managed-path-drift" { "drift" }
        "non-github-origin" { "non-gh" }
        "second-same-owner" { "second" }
        "different-owner" { "other" }
        "organization-rejects-classic-pat" { "org-pat" }
        default { throw "Unknown qualification scenario '$Name'" }
    }
    $target = Join-Path $qualificationRoot ("wf-e2e-" + $runRepositoryID + "-" + $repositorySuffix)
    New-Item -ItemType Directory -Force -Path $target | Out-Null
    & $Prepare $target
    $result = Invoke-DriverPhase $Name $target 'initial'
    if ($Name -in @("clean-new-repository","unrelated-dirty-files","second-same-owner")) {
        if ($result.platform_ready -ne $true -or $result.repository_admitted -ne $false) {
            $driverEvidence = "blocker='$([string]$result.blocker)', gate_kind='$([string]$result.gate_kind)', pull_request='$([string]$result.pull_request)'"
            throw "Scenario '$Name' bypassed the repository owner merge gate ($driverEvidence)"
        }
        Wait-ForOwnerMerge $result 'repository_onboarding'
        $result = Invoke-DriverPhase $Name $target 'onboarding-resume' ([string]$result.onboarding_plan_digest)
        if ($Name -eq 'clean-new-repository') {
            if ($result.platform_ready -ne $true -or $result.repository_admitted -ne $true -or [long]$result.delivery_plan -le 0 -or [long]$result.ticket -le 0) { throw "Scenario '$Name' did not reach the Worker pull request owner gate" }
            $deliveryPlan = [long]$result.delivery_plan
            $ticket = [long]$result.ticket
            Wait-ForOwnerMerge $result 'worker_delivery'
            $result = Invoke-DriverPhase $Name $target 'delivery-resume' '' $deliveryPlan $ticket
            if ($null -ne $result.gate_kind -or [string]$result.ticket_status -cne 'Delivered' -or [string]$result.plan_status -cne 'Completed') { throw "Scenario '$Name' did not reach Ticket Delivered and Plan Completed" }
            Assert-ControlPlaneCompletion $target $deliveryPlan $ticket
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
		$publishedDirtyRepository = "${runRepositoryPrefix}published"
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
    Remove-Item Env:GH_TOKEN -ErrorAction SilentlyContinue
    $workflowExecutable = Join-Path $workflowHome "bin\workflow.exe"
    if (Test-Path -LiteralPath $workflowExecutable -PathType Leaf) {
        try {
            & $workflowExecutable stop --workflow-home $workflowHome --timeout 30s | Out-Null
            $controlPlaneStopExit = $LASTEXITCODE
            if ($controlPlaneStopExit -ne 0) { $cleanupErrors.Add("Cannot stop the Control Plane before cleanup (exit $controlPlaneStopExit)") }
        } catch { $cleanupErrors.Add($_.Exception.Message) }
    }
	$env:GH_TOKEN = $cleanupToken
    $cleanupAfter = @()
    try {
        $cleanupAfter = @(gh repo list $GitHubOwner --limit 1000 --json nameWithOwner --jq ".[].nameWithOwner" | Where-Object { ([string]$_).StartsWith($runRepositoryPrefix, [StringComparison]::OrdinalIgnoreCase) })
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
