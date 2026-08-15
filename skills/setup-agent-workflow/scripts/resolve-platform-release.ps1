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
    Assert-ReleaseResolver ($Value -match '^(\d+)\.(\d+)\.(\d+)$') "$Description must be a stable semantic version"
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
$repository = [string]$policy.repository
Assert-ReleaseResolver ($repository -match '^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$') "Platform Release trust policy repository is invalid"
$keyName = [string]$policy.public_key_file
Assert-ReleaseResolver ($keyName -eq [IO.Path]::GetFileName($keyName)) "Platform Release trust policy public key identity is invalid"
$PublicKeyPath = Join-Path (Split-Path -Parent ([IO.Path]::GetFullPath($PolicyPath))) $keyName
Assert-ReleaseResolver (Test-Path -LiteralPath $PublicKeyPath -PathType Leaf) "Pinned Platform Release public key is missing: $PublicKeyPath"

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
Assert-ReleaseResolver ($tag -match '^platform-v(\d+\.\d+\.\d+)$') "Selected GitHub Release tag is not a stable Platform Release"
$metadataVersionText = [string]$Matches[1]
if (-not [string]::IsNullOrWhiteSpace($selectedVersionText)) {
    Assert-ReleaseResolver ($metadataVersionText -eq $selectedVersionText) "GitHub Release tag does not match the selected Platform Release version"
}
$assetNames = @($release.assets | ForEach-Object { [string]$_.name })
foreach ($fixedAsset in @("platform-release.json", "platform-release.json.sig")) {
    Assert-ReleaseResolver (@($assetNames | Where-Object { $_ -ceq $fixedAsset }).Count -eq 1) "GitHub Release lacks one exact '$fixedAsset' asset"
}

$taskTemp = Join-Path ([IO.Path]::GetTempPath()) ("workflow-platform-release-" + [Guid]::NewGuid().ToString("N"))
$manifestPath = Join-Path $taskTemp "platform-release.json"
$signaturePath = Join-Path $taskTemp "platform-release.json.sig"
try {
    New-Item -ItemType Directory -Path $taskTemp | Out-Null
    $downloadRoot = "https://github.com/$repository/releases/download/$tag"
    Invoke-WebRequest -Uri "$downloadRoot/platform-release.json" -Headers $headers -OutFile $manifestPath -UseBasicParsing
    Invoke-WebRequest -Uri "$downloadRoot/platform-release.json.sig" -Headers $headers -OutFile $signaturePath -UseBasicParsing

    $verificationArguments = @{ ManifestPath = $manifestPath; SignaturePath = $signaturePath }
    & (Join-Path $PSScriptRoot "verify-platform-release.ps1") @verificationArguments | Out-Null
    $manifest = Get-Content -LiteralPath $manifestPath -Raw | ConvertFrom-Json
    $candidateVersionText = [string]$manifest.release.version
    $candidateVersion = Get-StableVersion $candidateVersionText "Verified Platform Release version"
    Assert-ReleaseResolver ([string]$manifest.release.tag -eq $tag -and $candidateVersionText -eq $metadataVersionText) "Verified Platform Release identity does not match the selected GitHub Release"
    Assert-ReleaseResolver ([string]$release.target_commitish -ceq [string]$manifest.release.source_commit) "GitHub Release target commit does not match the verified Platform Release manifest"
    $manifestDigest = Get-SHA256File $manifestPath

    if ($installed) {
        $candidateComparison = $candidateVersion.CompareTo($durableVersion)
        Assert-ReleaseResolver ($candidateComparison -ge 0) "Verified Platform Release is older than the durable Platform Installation"
        if ($AllowUpgrade) {
            Assert-ReleaseResolver ($candidateComparison -gt 0) "AllowUpgrade authorizes only a version greater than the durable Platform Installation"
        } else {
            Assert-ReleaseResolver ($candidateComparison -eq 0) "A release change requires explicit AllowUpgrade authorization"
            Assert-ReleaseResolver ($manifestDigest -eq $durableDigest) "Durable Platform Installation repair requires its exact signed manifest digest"
        }
    }

    [ordered]@{
        verified = $true
        selection = $selection
        release_version = $candidateVersionText
        release_tag = $tag
        manifest_digest_sha256 = $manifestDigest
        manifest_path = [IO.Path]::GetFullPath($manifestPath)
        signature_path = [IO.Path]::GetFullPath($signaturePath)
        temp_directory = [IO.Path]::GetFullPath($taskTemp)
    } | ConvertTo-Json -Compress
} catch {
    if (Test-Path -LiteralPath $taskTemp) { Remove-Item -LiteralPath $taskTemp -Recurse -Force }
    throw
}
