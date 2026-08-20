$ErrorActionPreference = 'Stop'
$skillRoot = Split-Path $PSScriptRoot -Parent
$resolver = Join-Path $skillRoot 'scripts\resolve-workflow-candidate.ps1'
$scratch = Join-Path ([IO.Path]::GetTempPath()) ('workflow-candidate-resolution-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $scratch | Out-Null

try {
  $bundlePath = Join-Path $scratch 'workflow-windows-amd64.zip'
  $sbomPath = Join-Path $scratch 'worker-sbom.spdx.json'
  [IO.File]::WriteAllBytes($bundlePath, [Text.Encoding]::UTF8.GetBytes('candidate-bundle'))
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
      image = 'ghcr.io/skyhuang233/workflow-worker@sha256:' + ('c' * 64)
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
  $manifest | ConvertTo-Json -Depth 10 -Compress | Set-Content -LiteralPath (Join-Path $scratch 'workflow-release.json') -Encoding utf8NoBOM

  $resolved = & $resolver -CandidateDirectory $scratch -ExpectedVersion '0.0.1' -ExpectedSourceCommit $sourceCommit | ConvertFrom-Json
  if (-not [bool]$resolved.qualification_candidate -or [string]$resolved.bundle_sha256 -cne $bundleDigest) {
    throw 'Candidate resolver returned incorrect evidence'
  }
  $acceptedWrongSource = $true
  try { & $resolver -CandidateDirectory $scratch -ExpectedVersion '0.0.1' -ExpectedSourceCommit ('b' * 40) *> $null } catch { $acceptedWrongSource = $false }
  if ($acceptedWrongSource) { throw 'Candidate resolver accepted the wrong source commit' }

  [IO.File]::AppendAllText($bundlePath, 'tampered')
  $acceptedTamper = $true
  try { & $resolver -CandidateDirectory $scratch -ExpectedVersion '0.0.1' -ExpectedSourceCommit $sourceCommit *> $null } catch { $acceptedTamper = $false }
  if ($acceptedTamper) { throw 'Candidate resolver accepted a tampered Bundle' }

  Write-Output 'Workflow Release candidate resolution tests passed.'
} finally {
  Remove-Item -LiteralPath $scratch -Recurse -Force
}
