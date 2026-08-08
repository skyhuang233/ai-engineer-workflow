param(
  [Parameter(Mandatory = $true, Position = 0)]
  [ValidateRange(1, 1000000)]
  [int]$Iterations,

  [ValidateRange(1, 1024)]
  [int]$MaxParallelRuns = 1,

  [ValidateRange(1, 86400)]
  [int]$IntervalSeconds = 60,

  [string]$CodexAuthFile = ''
)

$ErrorActionPreference = 'Stop'

function Invoke-Native {
  param(
    [Parameter(Mandatory = $true)]
    [string]$Executable,
    [Parameter(ValueFromRemainingArguments = $true)]
    [string[]]$Arguments
  )

  & $Executable @Arguments
  if ($LASTEXITCODE -ne 0) {
    throw "$Executable exited with status $LASTEXITCODE"
  }
}

function Resolve-GitHubRepository {
  param([Parameter(Mandatory = $true)][string]$RemoteUrl)

  if ($RemoteUrl -notmatch 'github\.com[:/](?<owner>[^/:]+?)/(?<name>[^/]+?)(?:\.git)?$') {
    throw "origin is not a GitHub repository: $RemoteUrl"
  }
  return "$($matches.owner)/$($matches.name)"
}

function New-ControlToken {
  $bytes = New-Object byte[] 32
  $generator = [System.Security.Cryptography.RandomNumberGenerator]::Create()
  try {
    $generator.GetBytes($bytes)
  }
  finally {
    $generator.Dispose()
  }
  return [Convert]::ToBase64String($bytes)
}

function Assert-SourceCheckout {
  param([Parameter(Mandatory = $true)][string]$ProjectRoot)

  $branch = (& git -C $ProjectRoot branch --show-current).Trim()
  if ($LASTEXITCODE -ne 0 -or $branch -ne 'main') {
    throw "codex-afk.ps1 must run from the main branch; current branch is '$branch'"
  }
  $changes = @(& git -C $ProjectRoot status --porcelain) |
    Where-Object { $_ -notmatch '^\?\? \.worktrees[/\\]?$' }
  if ($LASTEXITCODE -ne 0 -or $changes.Count -gt 0) {
    throw "main must be clean before the Control Plane starts: $($changes -join ', ')"
  }
}

function Wait-Gateway {
  param(
    [Parameter(Mandatory = $true)]
    [System.Diagnostics.Process]$Process,
    [Parameter(Mandatory = $true)]
    [int]$Port,
    [Parameter(Mandatory = $true)]
    [string]$ErrorLog
  )

  $deadline = [DateTime]::UtcNow.AddSeconds(30)
  while ([DateTime]::UtcNow -lt $deadline) {
    if ($Process.HasExited) {
      $detail = if (Test-Path -LiteralPath $ErrorLog) {
        (Get-Content -LiteralPath $ErrorLog -Tail 20) -join [Environment]::NewLine
      }
      else {
        'no Gateway error log was produced'
      }
      throw "Gateway exited during startup with status $($Process.ExitCode): $detail"
    }

    $client = New-Object System.Net.Sockets.TcpClient
    try {
      $client.Connect('127.0.0.1', $Port)
      return
    }
    catch [System.Net.Sockets.SocketException] {
      Start-Sleep -Milliseconds 250
    }
    finally {
      $client.Dispose()
    }
  }
  throw "Gateway did not listen on port $Port within 30 seconds"
}

function Assert-GatewayPortAvailable {
  param([Parameter(Mandatory = $true)][int]$Port)

  $listeners = [System.Net.NetworkInformation.IPGlobalProperties]::GetIPGlobalProperties().GetActiveTcpListeners() |
    Where-Object { $_.Port -eq $Port }
  if (@($listeners).Count -gt 0) {
    throw "Gateway port $Port is already occupied; stop the existing listener before starting the Control Plane"
  }
}

function Stop-Gateway {
  param([System.Diagnostics.Process]$Process)

  if ($null -ne $Process -and -not $Process.HasExited) {
    Stop-Process -Id $Process.Id
    $Process.WaitForExit(10000) | Out-Null
  }
}

$scriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$projectRoot = (Resolve-Path -LiteralPath (Join-Path $scriptDir '..\..')).ProviderPath
$runtimeRoot = 'C:\ProgramData\workflow'
$database = Join-Path $runtimeRoot 'workflow.db'
$binaryPath = Join-Path $runtimeRoot 'bin\workflow.exe'
$workspaceRoot = Join-Path $runtimeRoot 'workspaces'
$stateRoot = Join-Path $runtimeRoot 'codex-state'
$logRoot = Join-Path $runtimeRoot 'logs'
$configPath = Join-Path $projectRoot 'config\toolchain.json'
$gatewayPort = 8787
$gatewayUrl = "http://host.docker.internal:$gatewayPort"
$gatewayControlUrl = "http://127.0.0.1:$gatewayPort"
$gatewayListen = "0.0.0.0:$gatewayPort"
$rootNumber = 2

if ([string]::IsNullOrWhiteSpace($CodexAuthFile)) {
  $codexHome = if (-not [string]::IsNullOrWhiteSpace($env:CODEX_HOME)) {
    $env:CODEX_HOME
  }
  else {
    Join-Path $env:USERPROFILE '.codex'
  }
  $CodexAuthFile = Join-Path $codexHome 'auth.json'
}
if (-not (Test-Path -LiteralPath $CodexAuthFile -PathType Leaf)) {
  throw "ChatGPT Codex authentication cache not found: $CodexAuthFile"
}
$CodexAuthFile = (Resolve-Path -LiteralPath $CodexAuthFile).ProviderPath

foreach ($command in 'git', 'go', 'docker') {
  if (-not (Get-Command $command -ErrorAction SilentlyContinue)) {
    throw "$command is required"
  }
}

Assert-SourceCheckout $projectRoot
$remoteUrl = (& git -C $projectRoot remote get-url origin).Trim()
if ($LASTEXITCODE -ne 0 -or [string]::IsNullOrWhiteSpace($remoteUrl)) {
  throw 'Cannot resolve the origin remote'
}
$repository = Resolve-GitHubRepository $remoteUrl

New-Item -ItemType Directory -Force -Path (Split-Path -Parent $binaryPath), $workspaceRoot, $stateRoot, $logRoot | Out-Null

$controlToken = New-ControlToken
$previousControlToken = $env:WORKFLOW_GATEWAY_CONTROL_TOKEN
$env:WORKFLOW_GATEWAY_CONTROL_TOKEN = $controlToken
$gatewayProcess = $null
$activeCommit = ''

try {
  for ($iteration = 1; $iteration -le $Iterations; $iteration++) {
    Write-Output ''
    Write-Output "===== Workflow control-plane iteration $iteration / $Iterations ====="
    Write-Output ''

    Assert-SourceCheckout $projectRoot
    Invoke-Native -Executable git -Arguments @('-C', $projectRoot, 'fetch', '--prune', 'origin', 'main')
    Invoke-Native -Executable git -Arguments @('-C', $projectRoot, 'merge', '--ff-only', 'origin/main')
    Assert-SourceCheckout $projectRoot
    $mainCommit = (& git -C $projectRoot rev-parse HEAD).Trim()
    if ($LASTEXITCODE -ne 0 -or [string]::IsNullOrWhiteSpace($mainCommit)) {
      throw 'Cannot resolve the current main commit'
    }

    if ($activeCommit -ne $mainCommit -or -not (Test-Path -LiteralPath $binaryPath)) {
      Stop-Gateway $gatewayProcess
      $gatewayProcess = $null
      Push-Location $projectRoot
      try {
        Invoke-Native -Executable go -Arguments @('build', '-trimpath', '-o', $binaryPath, './cmd/workflow')
      }
      finally {
        Pop-Location
      }

      $timestamp = [DateTime]::UtcNow.ToString('yyyyMMdd-HHmmss')
      $gatewayOutput = Join-Path $logRoot "gateway-$timestamp.out.log"
      $gatewayError = Join-Path $logRoot "gateway-$timestamp.err.log"
      $gatewayArguments = @(
        'gateway',
        '--config', $configPath,
        '--database', $database,
        '--listen', $gatewayListen
      )
      Assert-GatewayPortAvailable $gatewayPort
      $gatewayProcess = Start-Process -FilePath $binaryPath `
        -ArgumentList $gatewayArguments `
        -WorkingDirectory $projectRoot `
        -RedirectStandardOutput $gatewayOutput `
        -RedirectStandardError $gatewayError `
        -WindowStyle Hidden `
        -PassThru
      Wait-Gateway -Process $gatewayProcess -Port $gatewayPort -ErrorLog $gatewayError
      $activeCommit = $mainCommit
      Write-Output "Control Plane loaded from main commit $activeCommit."
      Write-Output "Gateway logs: $gatewayOutput and $gatewayError"
    }
    elseif ($gatewayProcess.HasExited) {
      throw "Gateway exited unexpectedly with status $($gatewayProcess.ExitCode)"
    }

    $pollArguments = @(
      'poll-github',
      '--config', $configPath,
      '--database', $database,
      '--repository', $repository,
      '--root', $rootNumber,
      '--source', $projectRoot,
      '--workspace-root', $workspaceRoot,
      '--state-root', $stateRoot,
      '--codex-auth-file', $CodexAuthFile,
      '--gateway-url', $gatewayUrl,
      '--gateway-control-url', $gatewayControlUrl,
      '--once',
      '--interval', "${IntervalSeconds}s",
      '--max-parallel-runs', $MaxParallelRuns
    )
    Invoke-Native $binaryPath @pollArguments

    if ($iteration -lt $Iterations) {
      Start-Sleep -Seconds $IntervalSeconds
    }
  }
}
finally {
  Stop-Gateway $gatewayProcess
  if ($null -eq $previousControlToken) {
    Remove-Item Env:\WORKFLOW_GATEWAY_CONTROL_TOKEN -ErrorAction SilentlyContinue
  }
  else {
    $env:WORKFLOW_GATEWAY_CONTROL_TOKEN = $previousControlToken
  }
}
