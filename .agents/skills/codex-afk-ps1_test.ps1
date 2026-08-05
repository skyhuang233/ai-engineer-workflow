$ErrorActionPreference = 'Stop'

$sandbox = Join-Path ([System.IO.Path]::GetTempPath()) ("workflow-afk-ps1-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $sandbox | Out-Null
try {
  $log = Join-Path $sandbox 'bash-arguments.txt'
  $stub = Join-Path $sandbox 'fake-bash.cmd'
  @'
@echo off
echo %* > "%WORKFLOW_AFK_PS1_TEST_LOG%"
exit /b %WORKFLOW_AFK_PS1_TEST_EXIT%
'@ | Set-Content -LiteralPath $stub -Encoding ascii

  $env:WORKFLOW_GIT_BASH = $stub
  $env:WORKFLOW_AFK_PS1_TEST_LOG = $log
  $env:WORKFLOW_AFK_PS1_TEST_EXIT = '23'
  $entrypoint = Join-Path $PSScriptRoot 'codex-afk.ps1'
  $hostExecutable = (Get-Process -Id $PID).Path
  $process = Start-Process -FilePath $hostExecutable -ArgumentList @('-NoProfile', '-File', $entrypoint, '37') -Wait -PassThru -NoNewWindow
  if ($process.ExitCode -ne 23) {
    throw "PowerShell AFK entrypoint exit code was $($process.ExitCode), expected 23"
  }
  $arguments = Get-Content -LiteralPath $log -Raw
  if ($arguments -notmatch '(?s)-lc .*\./codex-afk\.sh 37') {
    throw "PowerShell AFK entrypoint did not forward Iterations through Git Bash: $arguments"
  }
  Write-Output 'codex-afk PowerShell entrypoint test passed'
}
finally {
  Remove-Item -LiteralPath $sandbox -Recurse -Force -ErrorAction SilentlyContinue
  Remove-Item Env:WORKFLOW_GIT_BASH -ErrorAction SilentlyContinue
  Remove-Item Env:WORKFLOW_AFK_PS1_TEST_LOG -ErrorAction SilentlyContinue
  Remove-Item Env:WORKFLOW_AFK_PS1_TEST_EXIT -ErrorAction SilentlyContinue
}
