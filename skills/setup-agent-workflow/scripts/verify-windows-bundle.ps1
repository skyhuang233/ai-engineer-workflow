[CmdletBinding()]
param(
  [Parameter(Mandatory)][string]$BundlePath,
  [Parameter(Mandatory)][string]$ExpectedSHA256
)

$ErrorActionPreference = 'Stop'
if ((Split-Path -Leaf $BundlePath) -cne 'workflow-windows-amd64.zip') { throw 'Unexpected platform asset name' }
$actual = (Get-FileHash -LiteralPath $BundlePath -Algorithm SHA256).Hash.ToLowerInvariant()
$expected = $ExpectedSHA256.ToLowerInvariant() -replace '^sha256:', ''
if ($expected -notmatch '^[0-9a-f]{64}$' -or $actual -cne $expected) { throw 'Bundle SHA-256 does not match immutable Release metadata' }
Add-Type -AssemblyName System.IO.Compression.FileSystem
$zip = [IO.Compression.ZipFile]::OpenRead($BundlePath)
try {
  $entries = @($zip.Entries)
  $manifest = $entries | Where-Object { $_.FullName -ceq 'platform-release.json' }
  if (@($manifest).Count -ne 1) { throw 'Bundle requires one root platform-release.json' }
  foreach ($entry in $entries) {
    if ($entry.FullName.StartsWith('/') -or $entry.FullName.Contains('..') -or $entry.FullName.Contains('\')) { throw "Unsafe Bundle entry: $($entry.FullName)" }
  }
  $reader = [IO.StreamReader]::new($manifest.Open(), [Text.UTF8Encoding]::new($false), $true)
  try { $contract = $reader.ReadToEnd() | ConvertFrom-Json } finally { $reader.Dispose() }
  if ($contract.schema_version -ne 1 -or $contract.setup_protocol_version -ne 1) { throw 'Unsupported Bundle manifest/protocol schema' }
  if (@($contract.files).Count -lt 4) { throw 'Bundle inventory is incomplete' }
  $listed = @{}
  foreach ($file in $contract.files) { $listed[[string]$file.path] = $file }
  foreach ($required in @('setup/workflow-setup.exe','platform/workflow.exe')) { if (-not $listed.ContainsKey($required)) { throw "Bundle lacks $required" } }
  foreach ($entry in $entries | Where-Object { $_.FullName -cne 'platform-release.json' }) {
    if (-not $listed.ContainsKey($entry.FullName)) { throw "Unlisted Bundle entry: $($entry.FullName)" }
    $stream = $entry.Open(); try { $hash = [Security.Cryptography.SHA256]::Create().ComputeHash($stream) } finally { $stream.Dispose() }
    $hex = ([BitConverter]::ToString($hash)).Replace('-','').ToLowerInvariant()
    if ($hex -cne [string]$listed[$entry.FullName].sha256) { throw "Bundle digest mismatch: $($entry.FullName)" }
  }
  if ($entries.Count -ne ($listed.Count + 1)) { throw 'Bundle inventory does not exactly match archive' }
  [pscustomobject]@{version=$contract.version; bundle_digest=('sha256:'+$actual); manifest=$contract} | ConvertTo-Json -Depth 8 -Compress
} finally { $zip.Dispose() }
