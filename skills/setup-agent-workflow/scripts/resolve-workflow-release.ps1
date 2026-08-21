[CmdletBinding()]
param(
  [Parameter(Mandatory)][string]$DownloadDirectory
)

$ErrorActionPreference = 'Stop'
$policyPath = Join-Path (Split-Path $PSScriptRoot -Parent) 'trust\release-policy.json'
$policy = Get-Content -LiteralPath $policyPath -Raw | ConvertFrom-Json
$repository = [string]$policy.repository
if ([int]$policy.schema_version -ne 1) { throw 'Release policy schema is invalid' }
if ($repository -cnotmatch '^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$') { throw 'Release policy repository is invalid' }
if ([string]$policy.workflow_path -cne '.github/workflows/publish-workflow.yml') { throw 'Release policy publisher path is invalid' }
$minimumMatch = [regex]::Match([string]$policy.minimum_workflow_version, '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$')
if (-not $minimumMatch.Success) { throw 'Release policy minimum Workflow version is invalid' }
$minimumComponents = [Collections.Generic.List[int64]]::new()
foreach ($index in 1..3) {
  $component = 0L
  if (-not [int64]::TryParse($minimumMatch.Groups[$index].Value, [Globalization.NumberStyles]::None, [Globalization.CultureInfo]::InvariantCulture, [ref]$component) -or $component -gt 2147483647) {
    throw 'Release policy minimum Workflow version is invalid'
  }
  $minimumComponents.Add($component)
}

function Invoke-GhJSON {
  param([Parameter(Mandatory)][string]$Endpoint)
  $output = & gh api $Endpoint 2>&1
  if ($LASTEXITCODE -ne 0) { throw "GitHub API request failed for $Endpoint`: $output" }
  return ($output | Out-String | ConvertFrom-Json)
}

function Invoke-GhPagedJSON {
  param([Parameter(Mandatory)][string]$Endpoint)
  $output = & gh api --paginate --slurp $Endpoint 2>&1
  if ($LASTEXITCODE -ne 0) { throw "GitHub API request failed for $Endpoint`: $output" }
  $pages = @($output | Out-String | ConvertFrom-Json)
  $items = [Collections.Generic.List[object]]::new()
  foreach ($page in $pages) {
    foreach ($item in @($page)) { $items.Add($item) }
  }
  return $items
}

function Get-Asset {
  param([Parameter(Mandatory)]$Release, [Parameter(Mandatory)][string]$Name)
  $matches = @($Release.assets | Where-Object { [string]$_.name -ceq $Name })
  if ($matches.Count -ne 1) { throw "Release requires exactly one $Name asset" }
  $asset = $matches[0]
  if ([string]$asset.state -cne 'uploaded' -or [long]$asset.size -le 0) { throw "Release asset $Name is not completely uploaded" }
  if ([string]$asset.digest -cnotmatch '^sha256:[0-9a-f]{64}$') { throw "Release asset $Name lacks immutable SHA-256 metadata" }
  return $asset
}

function Download-ReleaseAsset {
  param([Parameter(Mandatory)][string]$Tag, [Parameter(Mandatory)][string]$Name)
  & gh release download $Tag --repo $repository --pattern $Name --dir $resolvedDownload --clobber
  if ($LASTEXITCODE -ne 0) { throw "Cannot download Workflow Release asset $Name" }
  $path = Join-Path $resolvedDownload $Name
  if (-not (Test-Path -LiteralPath $path -PathType Leaf)) { throw "Downloaded Workflow Release asset $Name is absent" }
  return $path
}

function Assert-DownloadedDigest {
  param([Parameter(Mandatory)][string]$Path, [Parameter(Mandatory)][string]$Expected, [Parameter(Mandatory)][long]$ExpectedSize, [Parameter(Mandatory)][string]$Label)
  $expectedHex = $Expected -replace '^sha256:', ''
  $actualHex = (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant()
  if ($actualHex -cne $expectedHex) { throw "$Label SHA-256 does not match immutable Release metadata" }
  if ($ExpectedSize -le 0 -or (Get-Item -LiteralPath $Path).Length -ne $ExpectedSize) { throw "$Label size does not match immutable Release metadata" }
  return $actualHex
}

$resolvedDownload = [IO.Path]::GetFullPath($DownloadDirectory)
New-Item -ItemType Directory -Force -Path $resolvedDownload | Out-Null
if (@(Get-ChildItem -LiteralPath $resolvedDownload -Force).Count -ne 0) { throw 'Workflow Release download directory must be empty' }

$releases = @(Invoke-GhPagedJSON "repos/$repository/releases?per_page=100")
$eligible = [Collections.Generic.List[object]]::new()
foreach ($release in $releases) {
  $match = [regex]::Match([string]$release.tag_name, '^workflow-v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$')
  if (-not $match.Success) { continue }
  if ([bool]$release.draft -or [bool]$release.prerelease -or -not [bool]$release.immutable -or [string]::IsNullOrWhiteSpace([string]$release.published_at)) { continue }
  $components = [Collections.Generic.List[int64]]::new()
  $validComponents = $true
  foreach ($index in 1..3) {
    $component = 0L
    if (-not [int64]::TryParse($match.Groups[$index].Value, [Globalization.NumberStyles]::None, [Globalization.CultureInfo]::InvariantCulture, [ref]$component) -or $component -gt 2147483647) {
      $validComponents = $false
      break
    }
    $components.Add($component)
  }
  if (-not $validComponents) { continue }
  $belowMinimum = $false
  foreach ($index in 0..2) {
    if ($components[$index] -lt $minimumComponents[$index]) { $belowMinimum = $true; break }
    if ($components[$index] -gt $minimumComponents[$index]) { break }
  }
  if ($belowMinimum) { continue }
  $eligible.Add([pscustomobject]@{Release=$release; Major=$components[0]; Minor=$components[1]; Patch=$components[2]})
}
if ($eligible.Count -eq 0) { throw 'No published immutable Workflow Release is available' }
$selected = $eligible | Sort-Object -Property @{Expression='Major';Descending=$true},@{Expression='Minor';Descending=$true},@{Expression='Patch';Descending=$true} | Select-Object -First 1
$release = $selected.Release
$expectedNames = @('workflow-windows-amd64.zip','workflow-release.json','worker-sbom.spdx.json')
if (@($release.assets).Count -ne $expectedNames.Count) { throw 'Workflow Release must contain exactly three assets' }
foreach ($asset in @($release.assets)) {
  if ([string]$asset.name -cnotin $expectedNames) { throw "Workflow Release contains unexpected asset $($asset.name)" }
}
$bundleAsset = Get-Asset $release 'workflow-windows-amd64.zip'
$manifestAsset = Get-Asset $release 'workflow-release.json'
$sbomAsset = Get-Asset $release 'worker-sbom.spdx.json'

# Authenticate and validate only the manifest before acquiring any other asset.
$manifestPath = Download-ReleaseAsset ([string]$release.tag_name) 'workflow-release.json'
$validator = Join-Path $PSScriptRoot 'verify-workflow-release-manifest.ps1'
$validatedJSON = & $validator -ManifestPath $manifestPath -ExpectedSHA256 ([string]$manifestAsset.digest) -ExpectedSize ([long]$manifestAsset.size) -ExpectedTag ([string]$release.tag_name)
if ($LASTEXITCODE -ne 0) { throw 'Workflow Release manifest bootstrap validation failed' }
$validated = $validatedJSON | ConvertFrom-Json
if ([string]$release.target_commitish -cne [string]$validated.source_commit) { throw 'Workflow Release target does not match its manifest source commit' }

$tagRef = Invoke-GhJSON "repos/$repository/git/ref/tags/$([Uri]::EscapeDataString([string]$release.tag_name))"
if ([string]$tagRef.object.type -cne 'commit' -or [string]$tagRef.object.sha -cne [string]$validated.source_commit) { throw 'Workflow Release tag does not resolve to its manifest source commit' }
$run = Invoke-GhJSON "repos/$repository/actions/runs/$($validated.github_actions_run_id)"
if ([string]$run.path -cne [string]$policy.workflow_path -or [string]$run.head_sha -cne [string]$validated.source_commit -or [string]$run.event -cne 'push' -or [string]$run.status -cne 'completed' -or [string]$run.conclusion -cne 'success') {
  throw 'Workflow Release source was not produced by the trusted successful publisher'
}

$bundlePath = Download-ReleaseAsset ([string]$release.tag_name) 'workflow-windows-amd64.zip'
$sbomPath = Download-ReleaseAsset ([string]$release.tag_name) 'worker-sbom.spdx.json'
$bundleMetadataDigest = Assert-DownloadedDigest $bundlePath ([string]$bundleAsset.digest) ([long]$bundleAsset.size) 'Bundle'
$sbomMetadataDigest = Assert-DownloadedDigest $sbomPath ([string]$sbomAsset.digest) ([long]$sbomAsset.size) 'Worker SBOM'
if ($bundleMetadataDigest -cne [string]$validated.bundle_sha256) { throw 'Bundle digest differs between Release metadata and Workflow Release manifest' }
if ($sbomMetadataDigest -cne [string]$validated.sbom_sha256) { throw 'Worker SBOM digest differs between Release metadata and Workflow Release manifest' }

[pscustomobject]@{
  schema_version = 1
  version = [string]$validated.version
  tag = [string]$release.tag_name
  source_commit = [string]$validated.source_commit
  manifest_path = $manifestPath
  manifest_sha256 = [string]$validated.manifest_sha256
  bundle_path = $bundlePath
  bundle_sha256 = $bundleMetadataDigest
  sbom_path = $sbomPath
  sbom_sha256 = $sbomMetadataDigest
  worker_image = [string]$validated.worker_image
} | ConvertTo-Json -Compress
