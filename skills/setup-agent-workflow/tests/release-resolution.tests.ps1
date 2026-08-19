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
  $manifestJSON = [IO.File]::ReadAllText($fixture).Replace(('b' * 64), $bundleDigest).Replace(('3' * 64), $sbomDigest)
  [IO.File]::WriteAllText($global:workflowResolutionFixture, $manifestJSON, [Text.UTF8Encoding]::new($false))
  $manifestDigest = (Get-FileHash -LiteralPath $global:workflowResolutionFixture -Algorithm SHA256).Hash.ToLowerInvariant()
  $releasePage = @(
    @{tag_name='platform-v99.0.0';draft=$false;prerelease=$false;immutable=$true;published_at='2026-08-18T00:00:00Z';assets=@()},
    @{tag_name='workflow-v0.0.0';draft=$false;prerelease=$false;immutable=$true;published_at='2026-08-18T00:00:00Z';assets=@()},
    @{tag_name='workflow-v0.0.1';draft=$false;prerelease=$false;immutable=$true;published_at='2026-08-19T00:00:00Z';assets=@(
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
      if ($endpoint -like 'repos/*/git/ref/tags/*') { Write-Output '{"object":{"type":"commit","sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}'; return }
      if ($endpoint -like 'repos/*/actions/runs/123') { Write-Output '{"path":".github/workflows/publish-workflow.yml","head_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","event":"push","status":"completed","conclusion":"success"}'; return }
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
  $downloads = @($global:workflowResolutionCalls | Where-Object { $_ -like 'release download*' })
  if ($downloads.Count -ne 3 -or $downloads[0] -notlike '*--pattern workflow-release.json*') { throw 'Resolver did not authenticate the manifest before downloading other assets' }

  $releaseObjects = @($global:workflowResolutionRelease | ConvertFrom-Json)
  $selected = @($releaseObjects | Where-Object tag_name -EQ 'workflow-v0.0.1')[0]
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
  Remove-Variable workflowResolutionFixture,workflowResolutionBundle,workflowResolutionSBOM,workflowResolutionRelease,workflowResolutionCalls -Scope Global -ErrorAction SilentlyContinue
  Remove-Item -LiteralPath $scratch -Recurse -Force
}
