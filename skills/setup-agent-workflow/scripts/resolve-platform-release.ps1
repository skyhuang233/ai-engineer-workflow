[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$HostFactsPath,
    [string]$Version = "",
    [switch]$AllowUpgrade
)

$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"
$PolicyPath = Join-Path $PSScriptRoot "..\trust\release-policy.json"

function Assert-ReleaseResolver([bool]$Condition, [string]$Message) {
    if (-not $Condition) { throw $Message }
}

function Get-StableVersion([string]$Value, [string]$Description) {
    Assert-ReleaseResolver ($Value -match '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$') "$Description must be a bare semantic version core (X.Y.Z) without leading zeros"
    return [Version]::new([int]$Matches[1], [int]$Matches[2], [int]$Matches[3])
}

function Get-SHA256File([string]$Path) {
    $hasher = [Security.Cryptography.SHA256]::Create()
    try { return ([BitConverter]::ToString($hasher.ComputeHash([IO.File]::ReadAllBytes($Path)))).Replace("-", "").ToLowerInvariant() } finally { $hasher.Dispose() }
}

foreach ($requiredPath in @($HostFactsPath, $PolicyPath)) {
    Assert-ReleaseResolver (Test-Path -LiteralPath $requiredPath -PathType Leaf) "Required release resolver input is missing: $requiredPath"
}
$policy = Get-Content -LiteralPath $PolicyPath -Raw | ConvertFrom-Json
Assert-ReleaseResolver ($policy.schema_version -eq 1) "Unsupported Platform Release trust policy"
$policyPropertyNames = @($policy.PSObject.Properties.Name | Sort-Object)
$expectedPolicyPropertyNames = @("minimum_platform_version", "repository", "schema_version", "workflow_path" | Sort-Object)
Assert-ReleaseResolver (($policyPropertyNames -join "`n") -ceq ($expectedPolicyPropertyNames -join "`n")) "Platform Release trust policy contains missing or unknown fields"
$repository = [string]$policy.repository
Assert-ReleaseResolver ($repository -match '^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$') "Platform Release trust policy repository is invalid"

$facts = Get-Content -LiteralPath $HostFactsPath -Raw | ConvertFrom-Json
Assert-ReleaseResolver ($facts.schema_version -eq 1) "Unsupported host-facts schema"
$installed = ($null -ne $facts.platform -and [bool]$facts.platform.installation_recorded)
$durableVersionText = ""
$durableVersion = $null
$durableDigest = ""
$selection = "latest-stable"
$selectedVersionText = $Version.Trim()
$pinlessExistingInstall = (-not $installed -and $null -ne $facts.workflow -and [bool]$facts.workflow.installed)

if (-not $installed) {
    Assert-ReleaseResolver (-not $AllowUpgrade) "AllowUpgrade applies only when upgrading an existing Platform Installation"
    if ($pinlessExistingInstall -and [string]::IsNullOrWhiteSpace($selectedVersionText)) {
        throw "An existing Workflow CLI has no verified Platform Release primary or backup pin. Recover it with -Version <exact-installed-version>; latest selection is allowed only for a true fresh install"
    }
    if ($pinlessExistingInstall) {
        Get-StableVersion $selectedVersionText "Exact recovery Platform Release version" | Out-Null
        $selection = "exact-version-recovery"
        $releaseAPI = "https://api.github.com/repos/$repository/releases/tags/platform-v$selectedVersionText"
    } elseif ([string]::IsNullOrWhiteSpace($selectedVersionText)) {
        $releaseAPI = "https://api.github.com/repos/$repository/releases/latest"
    } else {
        Get-StableVersion $selectedVersionText "Requested Platform Release version" | Out-Null
        $selection = "explicit-version"
        $releaseAPI = "https://api.github.com/repos/$repository/releases/tags/platform-v$selectedVersionText"
    }
} else {
    $durableVersionText = [string]$facts.platform.version
    $durableVersion = Get-StableVersion $durableVersionText "Durable Platform Installation version"
    $durableDigest = [string]$facts.platform.release_manifest_digest
    Assert-ReleaseResolver ($durableDigest -match '^[0-9a-f]{64}$') "Durable Platform Installation manifest digest is invalid"
    if ([string]::IsNullOrWhiteSpace($selectedVersionText)) {
        Assert-ReleaseResolver (-not $AllowUpgrade) "An upgrade requires an explicit Platform Release version"
        $selectedVersionText = $durableVersionText
        $selection = "durable-repair"
    } else {
        $requestedVersion = Get-StableVersion $selectedVersionText "Requested Platform Release version"
        $comparison = $requestedVersion.CompareTo($durableVersion)
        Assert-ReleaseResolver ($comparison -ge 0) "Requested Platform Release version is older than the durable Platform Installation"
        if ($AllowUpgrade) {
            Assert-ReleaseResolver ($comparison -gt 0) "AllowUpgrade authorizes only a version greater than the durable Platform Installation"
            $selection = "explicit-upgrade"
        } else {
            Assert-ReleaseResolver ($comparison -eq 0) "A greater Platform Release version requires explicit AllowUpgrade authorization"
            $selection = "durable-repair"
        }
    }
    $releaseAPI = "https://api.github.com/repos/$repository/releases/tags/platform-v$selectedVersionText"
}

$headers = @{ Accept = "application/vnd.github+json"; "X-GitHub-Api-Version" = "2022-11-28"; "User-Agent" = "agent-workflow-bootstrap" }
$releaseResponse = Invoke-WebRequest -Uri $releaseAPI -Headers $headers -UseBasicParsing
$release = [string]$releaseResponse.Content | ConvertFrom-Json
Assert-ReleaseResolver (-not [bool]$release.draft -and -not [bool]$release.prerelease) "Selected GitHub Release is not stable"
Assert-ReleaseResolver ($release.immutable -is [bool] -and [bool]$release.immutable) "Selected GitHub Release is not immutable"
$tag = [string]$release.tag_name
Assert-ReleaseResolver ($tag -match '^platform-v((0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*))$') "Selected GitHub Release tag is not a canonical stable Platform Release"
$metadataVersionText = [string]$Matches[1]
if (-not [string]::IsNullOrWhiteSpace($selectedVersionText)) {
    Assert-ReleaseResolver ($metadataVersionText -eq $selectedVersionText) "GitHub Release tag does not match the selected Platform Release version"
}
$fixedAssets = @("SHA256SUMS", "platform-provenance.json", "platform-release.json", "platform-sbom.spdx.json", "workflow-windows-amd64.zip" | Sort-Object)
$releaseAssets = @($release.assets)
$assetNames = @($releaseAssets | ForEach-Object { [string]$_.name } | Sort-Object)
Assert-ReleaseResolver (($assetNames -join "`n") -ceq ($fixedAssets -join "`n")) "GitHub Release asset set is not exact"
foreach ($asset in $releaseAssets) {
    $assetName = [string]$asset.name
    $expectedAssetURL = "https://github.com/$repository/releases/download/$tag/$assetName"
    Assert-ReleaseResolver ([string]$asset.browser_download_url -ceq $expectedAssetURL) "GitHub Release asset '$assetName' does not use its canonical HTTPS download URL"
}

$taskTemp = Join-Path ([IO.Path]::GetTempPath()) ("workflow-platform-release-" + [Guid]::NewGuid().ToString("N"))
$manifestPath = Join-Path $taskTemp "platform-release.json"
try {
    New-Item -ItemType Directory -Path $taskTemp | Out-Null
    $downloadRoot = "https://github.com/$repository/releases/download/$tag"
    Invoke-WebRequest -Uri "$downloadRoot/platform-release.json" -Headers $headers -OutFile $manifestPath -UseBasicParsing
    & (Join-Path $PSScriptRoot "verify-platform-release.ps1") -ManifestPath $manifestPath | Out-Null
    $manifest = Get-Content -LiteralPath $manifestPath -Raw | ConvertFrom-Json
    $candidateVersionText = [string]$manifest.release.version
    $candidateVersion = Get-StableVersion $candidateVersionText "Verified Platform Release version"
    Assert-ReleaseResolver ([string]$manifest.release.tag -eq $tag -and $candidateVersionText -eq $metadataVersionText) "Verified Platform Release identity does not match the selected GitHub Release"
    Assert-ReleaseResolver ([string]$release.target_commitish -ceq [string]$manifest.release.source_commit) "GitHub Release target commit does not match the verified Platform Release manifest"
    $sourceCommit = [string]$manifest.release.source_commit
    $runID = [long]$manifest.release.github_actions_run_id
    $runResponse = Invoke-WebRequest -Uri "https://api.github.com/repos/$repository/actions/runs/$runID" -Headers $headers -UseBasicParsing
    $run = [string]$runResponse.Content | ConvertFrom-Json
    Assert-ReleaseResolver ([long]$run.id -eq $runID -and [string]$run.repository.full_name -ceq $repository -and [string]$run.path -ceq [string]$policy.workflow_path -and [string]$run.head_sha -ceq $sourceCommit -and [string]$run.head_branch -ceq "main" -and [string]$run.event -ceq "push" -and [string]$run.status -ceq "completed" -and [string]$run.conclusion -ceq "success") "Platform Release publisher run does not match the canonical successful main workflow"

    $pullsResponse = Invoke-WebRequest -Uri "https://api.github.com/repos/$repository/commits/$sourceCommit/pulls" -Headers $headers -UseBasicParsing
    $matchingPulls = @([string]$pullsResponse.Content | ConvertFrom-Json | Where-Object { $null -ne $_.merged_at -and [string]$_.merge_commit_sha -ceq $sourceCommit -and [string]$_.base.ref -ceq "main" })
    Assert-ReleaseResolver ($matchingPulls.Count -eq 1 -and [long]$matchingPulls[0].number -gt 0) "Platform Release source commit must have exactly one merged main pull request"
    $pullNumber = [long]$matchingPulls[0].number
    $pullResponse = Invoke-WebRequest -Uri "https://api.github.com/repos/$repository/pulls/$pullNumber" -Headers $headers -UseBasicParsing
    $pull = [string]$pullResponse.Content | ConvertFrom-Json
    $repositoryOwner = $repository.Split('/')[0]
    $mergedByLogin = [string]$pull.merged_by.login
    Assert-ReleaseResolver ($null -ne $pull.merged_at -and [string]$pull.merge_commit_sha -ceq $sourceCommit -and [string]$pull.base.ref -ceq "main" -and [string]::Equals($mergedByLogin, $repositoryOwner, [StringComparison]::OrdinalIgnoreCase) -and [string]$pull.merged_by.type -ceq "User" -and -not $mergedByLogin.EndsWith("[bot]", [StringComparison]::OrdinalIgnoreCase)) "Platform Release source commit was not merged to main by the repository owner"
    $manifestDigest = Get-SHA256File $manifestPath

    if ($installed) {
        $candidateComparison = $candidateVersion.CompareTo($durableVersion)
        Assert-ReleaseResolver ($candidateComparison -ge 0) "Verified Platform Release is older than the durable Platform Installation"
        if ($AllowUpgrade) {
            Assert-ReleaseResolver ($candidateComparison -gt 0) "AllowUpgrade authorizes only a version greater than the durable Platform Installation"
        } else {
            Assert-ReleaseResolver ($candidateComparison -eq 0) "A release change requires explicit AllowUpgrade authorization"
            Assert-ReleaseResolver ($manifestDigest -eq $durableDigest) "Durable Platform Installation repair requires its exact manifest digest"
        }
    }

    [ordered]@{
        verified = $true
        selection = $selection
        release_version = $candidateVersionText
        release_tag = $tag
        manifest_digest_sha256 = $manifestDigest
        manifest_path = [IO.Path]::GetFullPath($manifestPath)
        temp_directory = [IO.Path]::GetFullPath($taskTemp)
    } | ConvertTo-Json -Compress
} catch {
    if (Test-Path -LiteralPath $taskTemp) { Remove-Item -LiteralPath $taskTemp -Recurse -Force }
    throw
}
