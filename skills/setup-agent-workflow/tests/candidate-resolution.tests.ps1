$ErrorActionPreference = 'Stop'
$skillRoot = Split-Path $PSScriptRoot -Parent
$resolver = Join-Path $skillRoot 'scripts\resolve-workflow-candidate.ps1'
$verifier = Join-Path $skillRoot 'scripts\verify-windows-bundle.ps1'
$extractor = Join-Path $skillRoot 'scripts\extract-verified-windows-bundle.ps1'
$scratch = Join-Path ([IO.Path]::GetTempPath()) ('workflow-candidate-resolution-' + [guid]::NewGuid().ToString('N'))
$candidateDirectory = Join-Path $scratch 'candidate'
$workingDirectory = Join-Path $scratch 'working'
New-Item -ItemType Directory -Path $candidateDirectory | Out-Null
New-Item -ItemType Directory -Path $workingDirectory | Out-Null

try {
  $bundlePath = Join-Path $candidateDirectory 'workflow-windows-amd64.zip'
  $sbomPath = Join-Path $candidateDirectory 'worker-sbom.spdx.json'
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
  $bundleManifest = [ordered]@{
    schema_version=1
    setup_protocol_version=1
    version='0.0.1'
    compatibility=[ordered]@{
      os='windows';architecture='amd64';database_schema=1;worker_image=$workerImage
      docker_desktop_version='4.86.0';docker_installer_url='https://example.test/docker.exe';docker_installer_sha256=('d'*64)
    }
    files=$files
  } | ConvertTo-Json -Depth 10 -Compress
  $allEntries = [ordered]@{'platform-release.json'=[Text.Encoding]::UTF8.GetBytes($bundleManifest)}
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
  [IO.File]::WriteAllBytes($sbomPath, [Text.Encoding]::UTF8.GetBytes('{"spdxVersion":"SPDX-2.3"}'))
  $bundleDigest = (Get-FileHash -LiteralPath $bundlePath -Algorithm SHA256).Hash.ToLowerInvariant()
  $sbomDigest = (Get-FileHash -LiteralPath $sbomPath -Algorithm SHA256).Hash.ToLowerInvariant()
  $sourceCommit = 'a' * 40
  $manifest = [ordered]@{
    schema_version = 1
    version = '0.0.1'
    source_commit = $sourceCommit
    github_actions_run_id = 42
    bundle = [ordered]@{name='workflow-windows-amd64.zip';sha256=$bundleDigest}
    worker = [ordered]@{
      image = $workerImage
      tools = [ordered]@{
        codex = [ordered]@{version='0.148.0'}
        github_cli = [ordered]@{version='2.97.0';linux_amd64_sha256=('d' * 64)}
        go = [ordered]@{version='1.26.6';linux_amd64_sha256=('e' * 64)}
        no_mistakes = [ordered]@{version='v1.41.2';repository='skyhuang233/no-mistakes';commit=('f' * 40)}
      }
    }
    sbom = [ordered]@{
      name='worker-sbom.spdx.json';format='spdx-json';sha256=$sbomDigest
      scan=[ordered]@{scanner='grype';severity_cutoff='high';only_fixed=$true}
    }
  }
  $manifestPath = Join-Path $candidateDirectory 'workflow-release.json'
  $manifest | ConvertTo-Json -Depth 10 -Compress | Set-Content -LiteralPath $manifestPath -Encoding utf8NoBOM

  $resolved = & $resolver -CandidateDirectory $candidateDirectory -ExpectedVersion '0.0.1' -ExpectedSourceCommit $sourceCommit | ConvertFrom-Json
  $manifestDigest = (Get-FileHash -LiteralPath $manifestPath -Algorithm SHA256).Hash.ToLowerInvariant()
  if (-not [bool]$resolved.qualification_candidate -or [string]$resolved.manifest_sha256 -cne $manifestDigest -or [string]$resolved.bundle_sha256 -cne $bundleDigest) {
    throw 'Candidate resolver returned incorrect evidence'
  }
  $verified = & $verifier -BundlePath $resolved.bundle_path -ExpectedSHA256 $resolved.bundle_sha256 -ExpectedVersion $resolved.version -ExpectedWorkerImage $resolved.worker_image | ConvertFrom-Json
  $prepared = & $extractor -BundlePath $resolved.bundle_path -WorkingDirectory $workingDirectory -VerifiedVersion $verified.version -VerifiedBundleDigest $verified.bundle_digest -ExpectedVersion $resolved.version -ExpectedSHA256 $resolved.bundle_sha256 | ConvertFrom-Json
  $verifiedExtractedBundle = [string]$prepared.extracted_bundle
  $launcher = Join-Path $verifiedExtractedBundle 'setup\workflow-setup.exe'
  if ([string]$prepared.version -cne '0.0.1' -or [string]$prepared.bundle_digest -cne ('sha256:' + $bundleDigest)) {
    throw 'Candidate extraction did not bind the verifier evidence'
  }
  if ((Split-Path -Parent $verifiedExtractedBundle) -cne [IO.Path]::GetFullPath($workingDirectory) -or -not (Test-Path -LiteralPath $launcher -PathType Leaf)) {
    throw 'Candidate acquisition did not reach the Launcher from the qualification working root'
  }
  if ([IO.File]::ReadAllText($launcher) -cne 'setup') {
    throw 'Candidate acquisition resolved the wrong Launcher bytes'
  }

  $acceptedWrongSource = $true
  try { & $resolver -CandidateDirectory $candidateDirectory -ExpectedVersion '0.0.1' -ExpectedSourceCommit ('b' * 40) *> $null } catch { $acceptedWrongSource = $false }
  if ($acceptedWrongSource) { throw 'Candidate resolver accepted the wrong source commit' }

  [IO.File]::AppendAllText($bundlePath, 'tampered')
  $acceptedTamper = $true
  try { & $resolver -CandidateDirectory $candidateDirectory -ExpectedVersion '0.0.1' -ExpectedSourceCommit $sourceCommit *> $null } catch { $acceptedTamper = $false }
  if ($acceptedTamper) { throw 'Candidate resolver accepted a tampered Bundle' }

  Write-Output 'Workflow Release candidate acquisition tests passed.'
} finally {
  Remove-Item -LiteralPath $scratch -Recurse -Force
}
