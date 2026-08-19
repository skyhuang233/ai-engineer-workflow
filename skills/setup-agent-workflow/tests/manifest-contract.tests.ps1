$ErrorActionPreference = 'Stop'
$scriptRoot = Split-Path $PSScriptRoot -Parent
$repositoryRoot = [IO.Path]::GetFullPath((Join-Path $scriptRoot '..\..'))
$validator = Join-Path $scriptRoot 'scripts\verify-workflow-release-manifest.ps1'
$fixtures = Join-Path $repositoryRoot 'internal\workflowrelease\testdata\manifest'

$valid = @(Get-ChildItem -LiteralPath (Join-Path $fixtures 'valid') -Filter '*.json')
if ($valid.Count -eq 0) { throw 'Shared manifest corpus has no valid fixtures' }
foreach ($fixture in $valid) {
  $scratch = Join-Path ([IO.Path]::GetTempPath()) ("workflow-manifest-contract-" + [guid]::NewGuid().ToString('N'))
  New-Item -ItemType Directory -Path $scratch | Out-Null
  try {
    $manifest = Join-Path $scratch 'workflow-release.json'
    Copy-Item -LiteralPath $fixture.FullName -Destination $manifest
    $digest = (Get-FileHash -LiteralPath $manifest -Algorithm SHA256).Hash.ToLowerInvariant()
    $result = & $validator -ManifestPath $manifest -ExpectedSHA256 ("sha256:" + $digest) -ExpectedSize (Get-Item $manifest).Length -ExpectedTag 'workflow-v0.0.1' | ConvertFrom-Json
    if ([string]$result.version -cne '0.0.1' -or [string]$result.manifest_sha256 -cne $digest) { throw "Valid fixture returned the wrong evidence: $($fixture.Name)" }
  } finally {
    Remove-Item -LiteralPath $scratch -Recurse -Force
  }
}

$invalid = @(Get-ChildItem -LiteralPath (Join-Path $fixtures 'invalid') -Filter '*.json')
if ($invalid.Count -eq 0) { throw 'Shared manifest corpus has no invalid fixtures' }
foreach ($fixture in $invalid) {
  $scratch = Join-Path ([IO.Path]::GetTempPath()) ("workflow-manifest-contract-" + [guid]::NewGuid().ToString('N'))
  New-Item -ItemType Directory -Path $scratch | Out-Null
  try {
    $manifest = Join-Path $scratch 'workflow-release.json'
    Copy-Item -LiteralPath $fixture.FullName -Destination $manifest
    $digest = (Get-FileHash -LiteralPath $manifest -Algorithm SHA256).Hash.ToLowerInvariant()
    $accepted = $true
    try { & $validator -ManifestPath $manifest -ExpectedSHA256 ("sha256:" + $digest) -ExpectedSize (Get-Item $manifest).Length -ExpectedTag 'workflow-v0.0.1' *> $null } catch { $accepted = $false }
    if ($accepted) { throw "Bootstrap validator accepted invalid shared fixture: $($fixture.Name)" }
  } finally {
    Remove-Item -LiteralPath $scratch -Recurse -Force
  }
}

$fixture = $valid[0]
$scratch = Join-Path ([IO.Path]::GetTempPath()) ("workflow-manifest-contract-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $scratch | Out-Null
try {
  $manifest = Join-Path $scratch 'workflow-release.json'
  Copy-Item -LiteralPath $fixture.FullName -Destination $manifest
  foreach ($case in @(
    @{Name='asset metadata mismatch'; Digest=('0' * 64); Tag='workflow-v0.0.1'},
    @{Name='tag version mismatch'; Digest=(Get-FileHash -LiteralPath $manifest -Algorithm SHA256).Hash.ToLowerInvariant(); Tag='workflow-v0.0.2'}
  )) {
    $accepted = $true
    try { & $validator -ManifestPath $manifest -ExpectedSHA256 $case.Digest -ExpectedSize (Get-Item $manifest).Length -ExpectedTag $case.Tag *> $null } catch { $accepted = $false }
    if ($accepted) { throw "Bootstrap validator accepted $($case.Name)" }
  }
} finally {
  Remove-Item -LiteralPath $scratch -Recurse -Force
}

Write-Output "Validated $($valid.Count) valid and $($invalid.Count) invalid shared Workflow Release manifest fixtures."
