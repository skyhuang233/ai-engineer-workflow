$ErrorActionPreference = 'Stop'
$skillRoot = Split-Path $PSScriptRoot -Parent
$verifier = Join-Path $skillRoot 'scripts\verify-windows-bundle.ps1'
$scratch = Join-Path ([IO.Path]::GetTempPath()) ("workflow-bundle-verification-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $scratch | Out-Null

try {
  $bundlePath = Join-Path $scratch 'workflow-windows-amd64.zip'
  $workerImage = 'ghcr.io/skyhuang233/workflow-worker@sha256:' + ('c' * 64)
  $payload = [ordered]@{
    'platform/workflow.exe' = [Text.Encoding]::UTF8.GetBytes('workflow')
    'repository-contract/repository.json' = [Text.Encoding]::UTF8.GetBytes('{}')
    'setup/workflow-setup.exe' = [Text.Encoding]::UTF8.GetBytes('setup')
    'skills/agent-workflow/SKILL.md' = [Text.Encoding]::UTF8.GetBytes('skill')
  }
  $files = @()
  foreach ($entry in $payload.GetEnumerator()) {
    $sum = [Security.Cryptography.SHA256]::HashData([byte[]]$entry.Value)
    $files += [ordered]@{path=$entry.Key;sha256=([Convert]::ToHexString($sum).ToLowerInvariant());size=$entry.Value.Length}
  }
  $manifest = [ordered]@{
    schema_version=1
    setup_protocol_version=1
    version='0.0.1'
    compatibility=[ordered]@{
      os='windows';architecture='amd64';database_schema=1;worker_image=$workerImage
      docker_desktop_version='4.86.0';docker_installer_url='https://example.test/docker.exe';docker_installer_sha256=('d'*64)
    }
    files=$files
  } | ConvertTo-Json -Depth 10 -Compress
  $allEntries = [ordered]@{'platform-release.json'=[Text.Encoding]::UTF8.GetBytes($manifest)}
  foreach ($entry in $payload.GetEnumerator()) { $allEntries[$entry.Key] = $entry.Value }
  $stream = [IO.File]::Create($bundlePath)
  $zip = [IO.Compression.ZipArchive]::new($stream, [IO.Compression.ZipArchiveMode]::Create, $false)
  try {
    foreach ($entry in $allEntries.GetEnumerator()) {
      $zipEntry = $zip.CreateEntry($entry.Key)
      $output = $zipEntry.Open()
      try { $output.Write($entry.Value, 0, $entry.Value.Length) } finally { $output.Dispose() }
    }
  } finally {
    $zip.Dispose()
    $stream.Dispose()
  }
  $digest = (Get-FileHash -LiteralPath $bundlePath -Algorithm SHA256).Hash.ToLowerInvariant()
  $result = & $verifier -BundlePath $bundlePath -ExpectedSHA256 $digest -ExpectedVersion '0.0.1' -ExpectedWorkerImage $workerImage | ConvertFrom-Json
  if ([string]$result.version -cne '0.0.1' -or [string]$result.bundle_digest -cne ('sha256:'+$digest)) { throw 'Bundle verifier returned incorrect evidence' }
  foreach ($case in @(
    @{Name='version mismatch';Version='0.0.2';Image=$workerImage},
    @{Name='Worker image mismatch';Version='0.0.1';Image=('ghcr.io/skyhuang233/workflow-worker@sha256:'+('e'*64))}
  )) {
    $accepted = $true
    try { & $verifier -BundlePath $bundlePath -ExpectedSHA256 $digest -ExpectedVersion $case.Version -ExpectedWorkerImage $case.Image *> $null } catch { $accepted = $false }
    if ($accepted) { throw "Bundle verifier accepted $($case.Name)" }
  }
  Write-Output 'Windows Bundle provenance, inventory, and digest tests passed.'
} finally {
  Remove-Item -LiteralPath $scratch -Recurse -Force
}
