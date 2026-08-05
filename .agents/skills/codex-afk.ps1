param(
  [Parameter(Mandatory = $true, Position = 0)]
  [ValidateRange(1, 1000000)]
  [int]$Iterations
)

function Resolve-GitBash {
  if ($env:WORKFLOW_GIT_BASH) {
    if (Test-Path -LiteralPath $env:WORKFLOW_GIT_BASH) {
      return (Resolve-Path -LiteralPath $env:WORKFLOW_GIT_BASH).ProviderPath
    }
    throw "Configured Git Bash not found: $($env:WORKFLOW_GIT_BASH)"
  }

  $candidates = New-Object System.Collections.Generic.List[string]
  $gitCommand = Get-Command git -ErrorAction SilentlyContinue

  if ($gitCommand) {
    $gitCmdDir = Split-Path -Parent $gitCommand.Source
    $gitRoot = Split-Path -Parent $gitCmdDir
    $candidates.Add((Join-Path $gitRoot 'bin\bash.exe'))
  }

  $candidates.Add('C:\Program Files\Git\bin\bash.exe')
  $candidates.Add('C:\Program Files (x86)\Git\bin\bash.exe')

  foreach ($candidate in $candidates) {
    if (Test-Path -LiteralPath $candidate) {
      return (Resolve-Path -LiteralPath $candidate).ProviderPath
    }
  }

  throw 'Git Bash not found. Install Git for Windows or add Git\bin\bash.exe to PATH.'
}

function Convert-ToMsysPath([string]$Path) {
  $resolved = (Resolve-Path -LiteralPath $Path).ProviderPath
  $normalized = $resolved -replace '\\', '/'

  if ($normalized -match '^([A-Za-z]):/(.*)$') {
    return "/$($matches[1].ToLower())/$($matches[2])"
  }

  throw "Unsupported Windows path for Git Bash: $resolved"
}

function Quote-Bash([string]$Value) {
  $singleQuoteEscape = [string][char]39 + [char]34 + [char]39 + [char]34 + [char]39
  return "'" + ($Value -replace "'", $singleQuoteEscape) + "'"
}

$scriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$bashExe = Resolve-GitBash
$bashDir = Convert-ToMsysPath $scriptDir
$command = "cd $(Quote-Bash $bashDir) && ./codex-afk.sh $Iterations"

& $bashExe -lc $command
exit $LASTEXITCODE
