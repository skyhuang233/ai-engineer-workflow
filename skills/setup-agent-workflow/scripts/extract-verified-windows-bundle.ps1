[CmdletBinding()]
param(
  [Parameter(Mandatory)][string]$BundlePath,
  [Parameter(Mandatory)][string]$WorkingDirectory,
  [Parameter(Mandatory)][string]$VerifiedVersion,
  [Parameter(Mandatory)][string]$VerifiedBundleDigest,
  [Parameter(Mandatory)][string]$ExpectedVersion,
  [Parameter(Mandatory)][string]$ExpectedSHA256
)

$ErrorActionPreference = 'Stop'

if (-not [IO.Path]::IsPathFullyQualified($WorkingDirectory)) {
  throw 'Bundle extraction working directory must be absolute'
}
$resolvedWorkingDirectory = [IO.Path]::GetFullPath($WorkingDirectory)
if (-not (Test-Path -LiteralPath $resolvedWorkingDirectory -PathType Container)) {
  throw 'Bundle extraction working directory does not exist'
}
if ((Split-Path -Leaf $BundlePath) -cne 'workflow-windows-amd64.zip') {
  throw 'Unexpected platform asset name'
}
if ($VerifiedVersion -cne $ExpectedVersion) {
  throw 'Verified Bundle version differs from the resolved Workflow Release'
}
if ($VerifiedBundleDigest -cnotmatch '^sha256:[0-9a-f]{64}$' -or $ExpectedSHA256 -cnotmatch '^[0-9a-f]{64}$') {
  throw 'Bundle verification evidence is invalid'
}
if ($VerifiedBundleDigest.Substring('sha256:'.Length) -cne $ExpectedSHA256) {
  throw 'Verified Bundle digest differs from the resolved Workflow Release'
}
$actualDigest = (Get-FileHash -LiteralPath $BundlePath -Algorithm SHA256).Hash.ToLowerInvariant()
if ($actualDigest -cne $ExpectedSHA256) {
  throw 'Bundle changed after verification'
}

$extractionDirectory = Join-Path $resolvedWorkingDirectory ('verified-workflow-bundle-' + [guid]::NewGuid().ToString('N'))
$created = $false
try {
  New-Item -ItemType Directory -Path $extractionDirectory | Out-Null
  $created = $true
  if (@(Get-ChildItem -LiteralPath $extractionDirectory -Force).Count -ne 0) {
    throw 'Bundle extraction directory must be empty'
  }
  Expand-Archive -LiteralPath $BundlePath -DestinationPath $extractionDirectory
  $launcher = Join-Path $extractionDirectory 'setup\workflow-setup.exe'
  if (-not (Test-Path -LiteralPath $launcher -PathType Leaf)) {
    throw 'Verified Bundle extraction does not contain the Launcher'
  }
  [pscustomobject]@{
    schema_version = 1
    version = $VerifiedVersion
    bundle_digest = $VerifiedBundleDigest
    extracted_bundle = $extractionDirectory
    launcher = $launcher
  } | ConvertTo-Json -Compress
} catch {
  if ($created) {
    Remove-Item -LiteralPath $extractionDirectory -Recurse -Force
  }
  throw
}
