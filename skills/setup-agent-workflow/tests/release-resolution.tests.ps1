$ErrorActionPreference = 'Stop'
$skillRoot = Split-Path $PSScriptRoot -Parent
$repositoryRoot = [IO.Path]::GetFullPath((Join-Path $skillRoot '..\..'))
$resolver = Join-Path $skillRoot 'scripts\resolve-workflow-release.ps1'
$fixture = Join-Path $repositoryRoot 'internal\workflowrelease\testdata\manifest\valid\schema-1.json'
$scratch = Join-Path ([IO.Path]::GetTempPath()) ("workflow-release-resolution-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $scratch | Out-Null

try {
  $global:workflowResolutionBundle = Join-Path $scratch 'bundle.bin'
  $global:workflowResolutionSBOM = Join-Path $scratch 'sbom.json'
  [IO.File]::WriteAllText($global:workflowResolutionBundle, 'bundle-bytes', [Text.UTF8Encoding]::new($false))
  [IO.File]::WriteAllText($global:workflowResolutionSBOM, '{"spdxVersion":"SPDX-2.3"}', [Text.UTF8Encoding]::new($false))
  $bundleDigest = (Get-FileHash -LiteralPath $global:workflowResolutionBundle -Algorithm SHA256).Hash.ToLowerInvariant()
  $sbomDigest = (Get-FileHash -LiteralPath $global:workflowResolutionSBOM -Algorithm SHA256).Hash.ToLowerInvariant()
  $global:workflowResolutionFixture = Join-Path $scratch 'workflow-release.json'
  $global:workflowResolutionQualificationCompletedAt = '2026-08-19T01:00:00Z'
  $global:workflowResolutionCandidateCommit = 'a' * 40
  $global:workflowResolutionSuccessfulPublisherAttempt = 3
  $manifestJSON = [IO.File]::ReadAllText($fixture).Replace(('b' * 64), $bundleDigest).Replace(('3' * 64), $sbomDigest)
  [IO.File]::WriteAllText($global:workflowResolutionFixture, $manifestJSON, [Text.UTF8Encoding]::new($false))
  $manifestDigest = (Get-FileHash -LiteralPath $global:workflowResolutionFixture -Algorithm SHA256).Hash.ToLowerInvariant()
  $releasePage = @(
    @{tag_name='platform-v99.0.0';draft=$false;prerelease=$false;immutable=$true;published_at='2026-08-18T00:00:00Z';assets=@()},
    @{tag_name='workflow-v0.0.0';draft=$false;prerelease=$false;immutable=$true;published_at='2026-08-18T00:00:00Z';assets=@()},
    @{tag_name='workflow-v0.0.1';target_commitish=('b' * 40);body="Immutable atomic Agent Workflow release.`n`nPublisher Run: 456`nPublisher Attempt: 2";author=@{login='github-actions[bot]';type='Bot'};draft=$false;prerelease=$false;immutable=$true;published_at='2026-08-19T00:00:00Z';assets=@(
      @{name='workflow-windows-amd64.zip';state='uploaded';size=(Get-Item $global:workflowResolutionBundle).Length;digest=('sha256:'+$bundleDigest)},
      @{name='workflow-release.json';state='uploaded';size=(Get-Item $global:workflowResolutionFixture).Length;digest=('sha256:'+$manifestDigest)},
      @{name='worker-sbom.spdx.json';state='uploaded';size=(Get-Item $global:workflowResolutionSBOM).Length;digest=('sha256:'+$sbomDigest)}
    )}
  )
  $global:workflowResolutionRelease = @(,$releasePage) | ConvertTo-Json -Depth 10 -Compress
  $global:workflowResolutionCalls = [Collections.Generic.List[string]]::new()

  function global:gh {
    $argv = @($args | ForEach-Object { [string]$_ })
    $global:workflowResolutionCalls.Add(($argv -join ' '))
    $global:LASTEXITCODE = 0
    if ($argv[0] -ceq 'api') {
      $endpoint = $argv[-1]
      if ($endpoint -like 'repos/*/releases?*') { Write-Output $global:workflowResolutionRelease; return }
      if ($endpoint -like 'repos/*/git/ref/tags/*') { Write-Output '{"object":{"type":"tag","sha":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"}}'; return }
      if ($endpoint -like 'repos/*/git/tags/*') { Write-Output '{"tag":"workflow-v0.0.1","message":"Workflow publisher provenance\nrun_id=456\nrun_attempt=2","object":{"type":"commit","sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}'; return }
      if ($endpoint -like 'repos/*/commits/*/pulls') { Write-Output '[{"number":7,"merged_at":"2026-08-19T02:00:00Z","merge_commit_sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","base":{"ref":"main"}}]'; return }
      if ($endpoint -like 'repos/*/pulls/7') { Write-Output (@{number=7;merged_at='2026-08-19T02:00:00Z';merge_commit_sha=('b'*40);base=@{ref='main'};head=@{ref='release-0.0.1';sha=$global:workflowResolutionCandidateCommit};merged_by=@{login='skyhuang233';type='User'}} | ConvertTo-Json -Depth 5 -Compress); return }
      if ($endpoint -like 'repos/*/git/commits/*') { Write-Output '{"parents":[{"sha":"cccccccccccccccccccccccccccccccccccccccc"},{"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}'; return }
      if ($endpoint -like 'repos/*/actions/workflows/worker-contract.yml') { Write-Output '{"id":2,"path":".github/workflows/worker-contract.yml","state":"active"}'; return }
      if ($endpoint -like 'repos/*/actions/runs/123/attempts/4') { Write-Output (@{id=123;run_attempt=4;workflow_id=2;path='.github/workflows/worker-contract.yml';head_sha=('a'*40);event='pull_request';status='completed';conclusion='success';updated_at=$global:workflowResolutionQualificationCompletedAt;pull_requests=@(@{number=7})} | ConvertTo-Json -Depth 5 -Compress); return }
      if ($endpoint -like 'repos/*/actions/workflows/publish-workflow.yml') { Write-Output '{"id":3,"path":".github/workflows/publish-workflow.yml","state":"active"}'; return }
      if ($endpoint -like 'repos/*/actions/runs/456/attempts/*') {
        $attempt = [long]($endpoint.Split('/')[-1])
        $conclusion = $(if ($attempt -eq $global:workflowResolutionSuccessfulPublisherAttempt) { 'success' } else { 'failure' })
        Write-Output (@{id=456;run_attempt=$attempt;workflow_id=3;path='.github/workflows/publish-workflow.yml';head_sha=('b'*40);head_branch='main';event='push';status='completed';conclusion=$conclusion} | ConvertTo-Json -Compress)
        return
      }
      if ($endpoint -like 'repos/*/actions/runs/456') { Write-Output '{"id":456,"run_attempt":4,"workflow_id":3,"path":".github/workflows/publish-workflow.yml","head_sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","head_branch":"main","event":"push","status":"completed","conclusion":"failure"}'; return }
      throw "Unexpected mocked gh api endpoint: $endpoint"
    }
    if ($argv[0] -ceq 'release' -and $argv[1] -ceq 'download') {
      $pattern = $argv[[Array]::IndexOf($argv, '--pattern') + 1]
      $directory = $argv[[Array]::IndexOf($argv, '--dir') + 1]
      $source = switch ($pattern) {
        'workflow-release.json' { $global:workflowResolutionFixture }
        'workflow-windows-amd64.zip' { $global:workflowResolutionBundle }
        'worker-sbom.spdx.json' { $global:workflowResolutionSBOM }
        default { throw "Unexpected mocked asset: $pattern" }
      }
      Copy-Item -LiteralPath $source -Destination (Join-Path $directory $pattern)
      return
    }
    throw "Unexpected mocked gh invocation: $($argv -join ' ')"
  }

  $download = Join-Path $scratch 'success'
  $result = & $resolver -DownloadDirectory $download | ConvertFrom-Json
  if ([string]$result.tag -cne 'workflow-v0.0.1') { throw 'Resolver did not select the highest eligible semantic version' }
  if ([string]$result.manifest_sha256 -cne $manifestDigest) { throw 'Resolver did not preserve the verified manifest digest' }
  if (@($global:workflowResolutionCalls | Where-Object { $_ -like 'api repos/*/actions/runs/456/attempts/3' }).Count -ne 1) { throw 'Resolver did not accept the historical successful publisher retry attempt' }
  $downloads = @($global:workflowResolutionCalls | Where-Object { $_ -like 'release download*' })
  if ($downloads.Count -ne 3 -or $downloads[0] -notlike '*--pattern workflow-release.json*') { throw 'Resolver did not authenticate the manifest before downloading other assets' }

  $releaseObjects = @($global:workflowResolutionRelease | ConvertFrom-Json)
  $selected = @($releaseObjects | Where-Object tag_name -EQ 'workflow-v0.0.1')[0]
  $selected.body = 'manually published'
  $global:workflowResolutionRelease = @(,$releaseObjects) | ConvertTo-Json -Depth 10 -Compress
  $global:workflowResolutionCalls.Clear()
  $accepted = $true
  try { & $resolver -DownloadDirectory (Join-Path $scratch 'target-mismatch') *> $null } catch { $accepted = $false }
  if ($accepted) { throw 'Resolver accepted a Release without exact publisher provenance' }
  if (@($global:workflowResolutionCalls | Where-Object { $_ -like 'release download*' }).Count -ne 1) { throw 'Resolver downloaded Bundle or SBOM before rejecting the Release target' }

  $selected.body = "Immutable atomic Agent Workflow release.`n`nPublisher Run: 456`nPublisher Attempt: 2"
  $global:workflowResolutionRelease = @(,$releaseObjects) | ConvertTo-Json -Depth 10 -Compress
  $global:workflowResolutionQualificationCompletedAt = '2026-08-19T03:00:00Z'
  $global:workflowResolutionCalls.Clear()
  $accepted = $true
  try { & $resolver -DownloadDirectory (Join-Path $scratch 'post-merge-qualification') *> $null } catch { $accepted = $false }
  if ($accepted) { throw 'Resolver accepted qualification completed after owner merge' }
  if (@($global:workflowResolutionCalls | Where-Object { $_ -like 'release download*' }).Count -ne 1) { throw 'Resolver downloaded Bundle or SBOM before rejecting post-merge qualification' }

  $global:workflowResolutionQualificationCompletedAt = '2026-08-19T01:00:00Z'
  $global:workflowResolutionCandidateCommit = 'd' * 40
  $global:workflowResolutionCalls.Clear()
  $accepted = $true
  try { & $resolver -DownloadDirectory (Join-Path $scratch 'candidate-head-mismatch') *> $null } catch { $accepted = $false }
  if ($accepted) { throw 'Resolver accepted an owner merge whose Pull Request head differs from the qualified candidate' }
  if (@($global:workflowResolutionCalls | Where-Object { $_ -like 'release download*' }).Count -ne 1) { throw 'Resolver downloaded Bundle or SBOM before rejecting candidate head mismatch' }

  $global:workflowResolutionCandidateCommit = 'a' * 40
  $selected.assets += [pscustomobject]@{name='unexpected.txt';state='uploaded';size=1;digest=('sha256:'+('4'*64))}
  $global:workflowResolutionRelease = @(,$releaseObjects) | ConvertTo-Json -Depth 10 -Compress
  $global:workflowResolutionCalls.Clear()
  $accepted = $true
  try { & $resolver -DownloadDirectory (Join-Path $scratch 'extra-asset') *> $null } catch { $accepted = $false }
  if ($accepted) { throw 'Resolver accepted an extra Workflow Release asset' }
  if (@($global:workflowResolutionCalls | Where-Object { $_ -like 'release download*' }).Count -ne 0) { throw 'Resolver downloaded bytes before rejecting the asset set' }

  Write-Output 'Workflow Release resolution ordering and exact-asset tests passed.'
} finally {
  Remove-Item Function:\global:gh -ErrorAction SilentlyContinue
  Remove-Variable workflowResolutionFixture,workflowResolutionBundle,workflowResolutionSBOM,workflowResolutionRelease,workflowResolutionCalls,workflowResolutionQualificationCompletedAt,workflowResolutionCandidateCommit,workflowResolutionSuccessfulPublisherAttempt -Scope Global -ErrorAction SilentlyContinue
  Remove-Item -LiteralPath $scratch -Recurse -Force
}
