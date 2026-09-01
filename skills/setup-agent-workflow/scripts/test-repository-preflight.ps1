[CmdletBinding()]
param(
  [string]$RepositoryPath = (Get-Location).Path
)

$ErrorActionPreference = 'Stop'
$repositoryPath = [IO.Path]::GetFullPath($RepositoryPath)
if (-not (Test-Path -LiteralPath $repositoryPath -PathType Container)) { throw 'Repository preflight path is not a directory' }

function Invoke-Tool {
  param([Parameter(Mandatory)][string]$Tool, [Parameter(Mandatory)][string[]]$Arguments, [Parameter(Mandatory)][string]$Failure)
  $output = & $Tool @Arguments 2>&1
  if ($LASTEXITCODE -ne 0) { throw "$Failure`: $output" }
  return ($output | Out-String).Trim()
}

Invoke-Tool -Tool 'gh' -Arguments @('auth','status','--hostname','github.com') -Failure 'Active github.com GitHub CLI login is required' | Out-Null
$gitRoot = ''
$branch = ''
$origin = ''
$inside = $false
Push-Location -LiteralPath $repositoryPath
try {
  $rootResult = & git rev-parse --show-toplevel 2>$null
  if ($LASTEXITCODE -eq 0) {
    $inside = $true
    $gitRoot = ($rootResult | Out-String).Trim()
    $branchResult = & git symbolic-ref --quiet --short HEAD 2>$null
    if ($LASTEXITCODE -ne 0) { throw 'Repository preflight blocks a detached HEAD' }
    $branch = ($branchResult | Out-String).Trim()
    $originResult = & git remote get-url origin 2>$null
    if ($LASTEXITCODE -eq 0) { $origin = ($originResult | Out-String).Trim() }
  }
} finally { Pop-Location }

$account = Invoke-Tool -Tool 'gh' -Arguments @('api','user','--jq','.login') -Failure 'Read active GitHub account'
$target = ''
if ($origin -match '^(?:git@github\.com:|ssh://git@github\.com/|https://github\.com/)([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+?)(?:\.git)?$') {
  $target = "$($Matches[1])/$($Matches[2])"
} else {
  $name = Split-Path -Leaf $(if ($inside) { $gitRoot } else { $repositoryPath })
  if ([string]::IsNullOrWhiteSpace($name)) { throw 'Cannot derive a GitHub repository name from the current directory' }
  $target = "$account/$name"
}

$exists = $false
$issuesEnabled = $false
$metadata = & gh api "repos/$target" 2>&1
if ($LASTEXITCODE -eq 0) {
  $repository = $metadata | Out-String | ConvertFrom-Json
  if ([string]$repository.full_name -cne $target) { throw 'GitHub returned a different repository than preflight resolved' }
  $exists = $true
  $issuesEnabled = [bool]$repository.has_issues
} elseif (($metadata | Out-String) -notmatch '(?i)404|not found') {
  throw "Read GitHub repository $target`: $metadata"
}

[pscustomobject]@{
  repository_path = $(if ($inside) { $gitRoot } else { $repositoryPath })
  in_git_worktree = $inside
  branch = $branch
  github_repository = $target
  repository_exists = $exists
  issues_enabled = $issuesEnabled
  origin = $origin
} | ConvertTo-Json -Compress
