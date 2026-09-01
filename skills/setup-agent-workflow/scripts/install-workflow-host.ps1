[CmdletBinding()]
param(
  [Parameter(Mandatory)][string]$WorkflowHome,
  [string]$DownloadDirectory
)

$ErrorActionPreference = 'Stop'
$policyPath = Join-Path (Split-Path $PSScriptRoot -Parent) 'trust\release-policy.json'
$policy = Get-Content -LiteralPath $policyPath -Raw | ConvertFrom-Json
if ([int]$policy.schema_version -ne 1 -or [string]$policy.repository -notmatch '^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$') { throw 'Workflow release policy is invalid' }

$isWindows = [Runtime.InteropServices.RuntimeInformation]::IsOSPlatform([Runtime.InteropServices.OSPlatform]::Windows)
$isMac = [Runtime.InteropServices.RuntimeInformation]::IsOSPlatform([Runtime.InteropServices.OSPlatform]::OSX)
if ($isWindows -and [Environment]::Is64BitOperatingSystem) { $assetName = 'workflow-windows-amd64.exe'; $hostName = 'workflow.exe' }
elseif ($isMac -and [Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString() -eq 'Arm64') { $assetName = 'workflow-darwin-arm64'; $hostName = 'workflow' }
else { throw 'Workflow Host Executable supports only Windows x64 and macOS Apple Silicon' }

function Invoke-GhJson {
  param([string[]]$Arguments)
  $output = & gh @Arguments 2>&1
  if ($LASTEXITCODE -ne 0) { throw "GitHub request failed: $output" }
  return $output | Out-String | ConvertFrom-Json
}

$releases = @(Invoke-GhJson @('api','--paginate','--slurp',"repos/$($policy.repository)/releases?per_page=100")) | ForEach-Object { $_ } | ForEach-Object { $_ }
$eligible = @($releases | Where-Object { $_.tag_name -match '^workflow-v\d+\.\d+\.\d+$' -and -not $_.draft -and -not $_.prerelease -and $_.immutable -and $_.published_at })
if ($eligible.Count -eq 0) { throw 'No published immutable Workflow Release is available' }
$release = $eligible | Sort-Object { [version]($_.tag_name -replace '^workflow-v','') } -Descending | Select-Object -First 1
$asset = @($release.assets | Where-Object { $_.name -ceq $assetName })
if ($asset.Count -ne 1 -or [string]$asset[0].digest -cnotmatch '^sha256:[0-9a-f]{64}$' -or [long]$asset[0].size -le 0) { throw "Release $($release.tag_name) lacks one digest-addressed $assetName asset" }

if ([string]::IsNullOrWhiteSpace($DownloadDirectory)) { $DownloadDirectory = [IO.Path]::GetTempPath() }
$downloadRoot = Join-Path ([IO.Path]::GetFullPath($DownloadDirectory)) ([guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $downloadRoot -Force | Out-Null
try {
  & gh release download $release.tag_name --repo $policy.repository --pattern $assetName --dir $downloadRoot
  if ($LASTEXITCODE -ne 0) { throw 'Download Host Executable failed' }
  $downloaded = Join-Path $downloadRoot $assetName
  if (-not (Test-Path -LiteralPath $downloaded -PathType Leaf) -or (Get-Item -LiteralPath $downloaded).Length -ne [long]$asset[0].size) { throw 'Downloaded Host Executable has an unexpected size' }
  $actual = (Get-FileHash -LiteralPath $downloaded -Algorithm SHA256).Hash.ToLowerInvariant()
  if ($actual -cne ([string]$asset[0].digest).Substring(7)) { throw 'Downloaded Host Executable digest differs from Release metadata' }
  $bin = Join-Path ([IO.Path]::GetFullPath($WorkflowHome)) 'bin'
  New-Item -ItemType Directory -Path $bin -Force | Out-Null
  $temporary = Join-Path $bin ('.workflow-' + [guid]::NewGuid().ToString('N') + '.tmp')
  Copy-Item -LiteralPath $downloaded -Destination $temporary -Force
  if ($isMac) { & chmod u+x $temporary; if ($LASTEXITCODE -ne 0) { throw 'Mark downloaded macOS Host Executable executable' } }
  Move-Item -LiteralPath $temporary -Destination (Join-Path $bin $hostName) -Force
  [pscustomobject]@{ executable_path = (Join-Path $bin $hostName); version = ($release.tag_name -replace '^workflow-v','') } | ConvertTo-Json -Compress
} finally { Remove-Item -LiteralPath $downloadRoot -Force -Recurse -ErrorAction SilentlyContinue }
