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
    $parts = @([string]$Matches[1], [string]$Matches[2], [string]$Matches[3])
    foreach ($part in $parts) {
        $number = [uint64]0
        Assert-ReleaseResolver ([uint64]::TryParse($part, [ref]$number) -and $number -le [int]::MaxValue) "$Description components must fit the signed 32-bit range"
    }
    return [Version]::new([int]$parts[0], [int]$parts[1], [int]$parts[2])
}

function Get-SHA256File([string]$Path) {
    $hasher = [Security.Cryptography.SHA256]::Create()
    try { return ([BitConverter]::ToString($hasher.ComputeHash([IO.File]::ReadAllBytes($Path)))).Replace("-", "").ToLowerInvariant() } finally { $hasher.Dispose() }
}

foreach ($required in @($HostFactsPath, $PolicyPath)) {
    Assert-ReleaseResolver (Test-Path -LiteralPath $required -PathType Leaf) "Required release resolver input is missing: $required"
}

# The caller supplies this one-use credential on standard input. It is never
# read from gh, host state, or a persisted Control Plane credential.
$pat = [string]([Console]::In.ReadLine())
if ($pat.Length -gt 0 -and $pat[0] -eq [char]0xFEFF) { $pat = $pat.Substring(1) }
Assert-ReleaseResolver (-not [string]::IsNullOrWhiteSpace($pat)) "Platform Release resolution requires a GitHub PAT on standard input"
$policy = Get-Content -LiteralPath $PolicyPath -Raw | ConvertFrom-Json
Assert-ReleaseResolver ($policy.schema_version -eq 1 -and [string]$policy.repository -match '^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$') "Platform Release trust policy is invalid"
$repository = [string]$policy.repository
$headers = @{ Authorization = "Bearer $pat"; Accept = "application/vnd.github+json"; "X-GitHub-Api-Version" = "2022-11-28"; "User-Agent" = "agent-workflow-bootstrap" }
$requiredAssets = @("SHA256SUMS", "platform-release.json", "workflow-windows-amd64.zip")

function Test-CanonicalPlatformRelease($Candidate) {
    if ($null -eq $Candidate -or [bool]$Candidate.draft -or [bool]$Candidate.prerelease -or -not [bool]$Candidate.immutable) { return $false }
    if ([string]$Candidate.tag_name -notmatch '^platform-v((0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*))$') { return $false }
    try { $null = Get-StableVersion ([string]$Matches[1]) "Platform Release version" } catch { return $false }
    foreach ($required in $requiredAssets) {
        $matches = @($Candidate.assets | Where-Object { [string]$_.name -ceq $required })
        if ($matches.Count -ne 1 -or [long]$matches[0].id -le 0) { return $false }
    }
    return $true
}

function Get-ReleaseAsset($Release, [string]$Name) {
    $asset = @($Release.assets | Where-Object { [string]$_.name -ceq $Name })
    Assert-ReleaseResolver ($asset.Count -eq 1 -and [long]$asset[0].id -gt 0) "Platform Release lacks exact asset '$Name'"
    return $asset[0]
}

function Download-ReleaseAsset($Asset, [string]$Destination) {
    $assetHeaders = @{ Authorization = "Bearer $pat"; Accept = "application/octet-stream"; "X-GitHub-Api-Version" = "2022-11-28"; "User-Agent" = "agent-workflow-bootstrap" }
    Invoke-WebRequest -Uri "https://api.github.com/repos/$repository/releases/assets/$([long]$Asset.id)" -Headers $assetHeaders -OutFile $Destination -UseBasicParsing
}

function Get-AdditionalAssetWarnings($Release) {
    $warnings = @()
    foreach ($asset in @($Release.assets)) {
        $name = [string]$asset.name
        if ($requiredAssets -ccontains $name) { continue }
        if ([string]::IsNullOrWhiteSpace($name)) {
            $warnings += "Platform Release includes an additional asset without a usable name; ignored"
        } elseif ([long]$asset.id -le 0) {
            $warnings += "Platform Release includes additional asset '$name' missing a usable asset ID; ignored"
        } else {
            $warnings += "Platform Release includes additional asset '$name'; ignored"
        }
    }
    return @($warnings)
}

$facts = Get-Content -LiteralPath $HostFactsPath -Raw | ConvertFrom-Json
Assert-ReleaseResolver ($facts.schema_version -eq 1) "Unsupported host-facts schema"
$installed = ($null -ne $facts.platform -and [bool]$facts.platform.installation_recorded)
$selected = $Version.Trim()
$selection = "latest-stable"
$durableVersion = $null; $durableDigest = ""
if ($installed) {
    $durableVersion = Get-StableVersion ([string]$facts.platform.version) "Durable Platform Installation version"
    $durableDigest = [string]$facts.platform.release_manifest_digest
    Assert-ReleaseResolver ($durableDigest -match '^[0-9a-f]{64}$') "Durable Platform Installation manifest digest is invalid"
    if ([string]::IsNullOrWhiteSpace($selected)) { Assert-ReleaseResolver (-not $AllowUpgrade) "An upgrade requires an explicit Platform Release version"; $selected = $durableVersion.ToString(3); $selection = "durable-repair" }
    else {
        $comparison = (Get-StableVersion $selected "Requested Platform Release version").CompareTo($durableVersion)
        Assert-ReleaseResolver ($comparison -ge 0) "Requested Platform Release version is older than the durable Platform Installation"
        if ($AllowUpgrade) { Assert-ReleaseResolver ($comparison -gt 0) "AllowUpgrade authorizes only a version greater than the durable Platform Installation"; $selection = "explicit-upgrade" } else { Assert-ReleaseResolver ($comparison -eq 0) "A greater Platform Release version requires explicit AllowUpgrade authorization"; $selection = "durable-repair" }
    }
} elseif (-not [string]::IsNullOrWhiteSpace($selected)) {
    Get-StableVersion $selected "Requested Platform Release version" | Out-Null
    $selection = "explicit-version"
} elseif ($null -ne $facts.workflow -and [bool]$facts.workflow.installed) {
    throw "An existing Workflow CLI without a recorded Platform Installation requires -Version <exact-installed-version>"
}

try {
    if ([string]::IsNullOrWhiteSpace($selected)) {
        $release = $null; $highest = $null
        foreach ($page in 1..100) {
            $response = Invoke-WebRequest -Uri "https://api.github.com/repos/$repository/releases?per_page=100&page=$page" -Headers $headers -UseBasicParsing
            $candidates = @([string]$response.Content | ConvertFrom-Json)
            if ($candidates.Count -eq 0) { break }
            foreach ($candidate in $candidates) {
                if (-not (Test-CanonicalPlatformRelease $candidate)) { continue }
                $candidateTag = [string]$candidate.tag_name
                $candidateVersion = Get-StableVersion $candidateTag.Substring(10) "Platform Release version"
                if ($null -eq $highest -or $candidateVersion.CompareTo($highest) -gt 0) { $release = $candidate; $highest = $candidateVersion }
            }
        }
        Assert-ReleaseResolver ($null -ne $release) "No canonical immutable stable Platform Release was found"
    } else {
        $response = Invoke-WebRequest -Uri "https://api.github.com/repos/$repository/releases/tags/platform-v$selected" -Headers $headers -UseBasicParsing
        $release = [string]$response.Content | ConvertFrom-Json
    }
    Assert-ReleaseResolver (Test-CanonicalPlatformRelease $release) "Selected GitHub Release is not a canonical immutable stable Platform Release"
    $tag = [string]$release.tag_name
    $versionText = $tag.Substring(10)
    if (-not [string]::IsNullOrWhiteSpace($selected)) { Assert-ReleaseResolver ($versionText -ceq $selected) "GitHub Release tag does not match the selected Platform Release version" }
    $taskTemp = Join-Path ([IO.Path]::GetTempPath()) ("workflow-platform-release-" + [Guid]::NewGuid().ToString("N"))
    New-Item -ItemType Directory -Path $taskTemp | Out-Null
    $manifestPath = Join-Path $taskTemp "platform-release.json"
    Download-ReleaseAsset (Get-ReleaseAsset $release "platform-release.json") $manifestPath
    & (Join-Path $PSScriptRoot "verify-platform-release.ps1") -ManifestPath $manifestPath | Out-Null
    $manifest = Get-Content -LiteralPath $manifestPath -Raw | ConvertFrom-Json
    Assert-ReleaseResolver ([string]$manifest.release.repository -ceq $repository -and [string]$manifest.release.tag -ceq $tag -and [string]$manifest.release.version -ceq $versionText) "Platform Release manifest does not match selected release"
    $manifestDigest = Get-SHA256File $manifestPath
    if ($installed -and -not $AllowUpgrade) { Assert-ReleaseResolver ($manifestDigest -ceq $durableDigest) "Durable Platform Installation repair requires its exact manifest digest" }
    [ordered]@{ verified = $true; selection = $selection; release_version = $versionText; release_tag = $tag; manifest_digest_sha256 = $manifestDigest; manifest_path = [IO.Path]::GetFullPath($manifestPath); temp_directory = [IO.Path]::GetFullPath($taskTemp); asset_warnings = @(Get-AdditionalAssetWarnings $release) } | ConvertTo-Json -Compress
} catch {
    if ($taskTemp -and (Test-Path -LiteralPath $taskTemp)) { Remove-Item -LiteralPath $taskTemp -Recurse -Force }
    throw
} finally { $pat = $null }
