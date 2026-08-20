[CmdletBinding()]
param(
  [Parameter(Mandatory)][string]$CandidateDirectory,
  [Parameter(Mandatory)][string]$ExpectedVersion,
  [Parameter(Mandatory)][string]$ExpectedSourceCommit
)

$ErrorActionPreference = 'Stop'

if ($ExpectedVersion -cnotmatch '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$') {
  throw 'Candidate version must be a bare semantic version core'
}
if ($ExpectedSourceCommit -cnotmatch '^[0-9a-f]{40}$') {
  throw 'Candidate source commit must be a full lowercase Git commit'
}

$resolvedCandidate = [IO.Path]::GetFullPath($CandidateDirectory)
if (-not (Test-Path -LiteralPath $resolvedCandidate -PathType Container)) {
  throw 'Workflow Release candidate directory does not exist'
}
$expectedNames = @('worker-sbom.spdx.json','workflow-release.json','workflow-windows-amd64.zip')
$actualNames = @(Get-ChildItem -LiteralPath $resolvedCandidate -Force -File | Sort-Object Name | ForEach-Object Name)
if (($actualNames -join ' ') -cne ($expectedNames -join ' ')) {
  throw 'Workflow Release candidate directory must contain exactly the three release assets'
}
if (@(Get-ChildItem -LiteralPath $resolvedCandidate -Force -Directory).Count -ne 0) {
  throw 'Workflow Release candidate directory contains an unexpected subdirectory'
}

$manifestPath = Join-Path $resolvedCandidate 'workflow-release.json'
$bundlePath = Join-Path $resolvedCandidate 'workflow-windows-amd64.zip'
$sbomPath = Join-Path $resolvedCandidate 'worker-sbom.spdx.json'
foreach ($path in @($manifestPath,$bundlePath,$sbomPath)) {
  if ((Get-Item -LiteralPath $path).Length -le 0) { throw "Workflow Release candidate asset is empty: $(Split-Path -Leaf $path)" }
}

$manifestDigest = (Get-FileHash -LiteralPath $manifestPath -Algorithm SHA256).Hash.ToLowerInvariant()
$validator = Join-Path $PSScriptRoot 'verify-workflow-release-manifest.ps1'
$validated = & $validator `
  -ManifestPath $manifestPath `
  -ExpectedSHA256 ('sha256:' + $manifestDigest) `
  -ExpectedSize (Get-Item -LiteralPath $manifestPath).Length `
  -ExpectedTag ('workflow-v' + $ExpectedVersion) | ConvertFrom-Json
if ([string]$validated.source_commit -cne $ExpectedSourceCommit) {
  throw 'Workflow Release candidate manifest does not match the expected source commit'
}

$bundleDigest = (Get-FileHash -LiteralPath $bundlePath -Algorithm SHA256).Hash.ToLowerInvariant()
$sbomDigest = (Get-FileHash -LiteralPath $sbomPath -Algorithm SHA256).Hash.ToLowerInvariant()
if ($bundleDigest -cne [string]$validated.bundle_sha256) {
  throw 'Candidate Bundle SHA-256 differs from its Workflow Release manifest'
}
if ($sbomDigest -cne [string]$validated.sbom_sha256) {
  throw 'Candidate Worker SBOM SHA-256 differs from its Workflow Release manifest'
}

[pscustomobject]@{
  schema_version = 1
  qualification_candidate = $true
  version = [string]$validated.version
  source_commit = [string]$validated.source_commit
  manifest_path = $manifestPath
  bundle_path = $bundlePath
  bundle_sha256 = $bundleDigest
  sbom_path = $sbomPath
  sbom_sha256 = $sbomDigest
  worker_image = [string]$validated.worker_image
} | ConvertTo-Json -Compress
