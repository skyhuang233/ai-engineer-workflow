[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$Repository,
    [string]$WorkflowHome = ""
)

$ErrorActionPreference = "Stop"
function Get-SHA256File([string]$Path) {
    $hasher = [Security.Cryptography.SHA256]::Create()
    try { return ([BitConverter]::ToString($hasher.ComputeHash([IO.File]::ReadAllBytes($Path)))).Replace("-", "").ToLowerInvariant() } finally { $hasher.Dispose() }
}
$repoPath = [System.IO.Path]::GetFullPath($Repository)
if (-not (Test-Path -LiteralPath $repoPath -PathType Container)) {
    throw "Repository directory does not exist: $repoPath"
}

if ([string]::IsNullOrWhiteSpace($WorkflowHome)) {
    if ([string]::IsNullOrWhiteSpace($env:LOCALAPPDATA)) {
        throw "LOCALAPPDATA is required"
    }
    $WorkflowHome = Join-Path $env:LOCALAPPDATA "AgentWorkflow"
}
$WorkflowHome = [System.IO.Path]::GetFullPath($WorkflowHome)
if ($WorkflowHome.StartsWith("\\")) {
    throw "Workflow Home must be a local path"
}

function Invoke-ObservedCommand([string]$Name, [string[]]$Arguments) {
    $command = Get-Command $Name -ErrorAction SilentlyContinue
    if ($null -eq $command) {
        return [ordered]@{ installed = $false; output = ""; exit_code = $null }
    }
    $previousPreference = $ErrorActionPreference
    try {
        $ErrorActionPreference = "Continue"
        $output = & $command.Source @Arguments 2>&1 | Out-String
        $exitCode = $LASTEXITCODE
    } finally {
        $ErrorActionPreference = $previousPreference
    }
    return [ordered]@{ installed = $true; output = $output.Trim(); exit_code = $exitCode }
}

$git = Invoke-ObservedCommand "git" @("-C", $repoPath, "rev-parse", "--show-toplevel")
$isRepository = $git.installed -and $git.exit_code -eq 0
$gitFacts = [ordered]@{ installed = $git.installed; is_repository = $isRepository }
if ($isRepository) {
    $gitFacts.root = $git.output
    $gitFacts.branch = (& git -C $repoPath branch --show-current 2>$null | Out-String).Trim()
    $gitFacts.head = (& git -C $repoPath rev-parse --verify HEAD 2>$null | Out-String).Trim()
    $gitFacts.origin = (& git -C $repoPath remote get-url origin 2>$null | Out-String).Trim()
    $gitFacts.status_porcelain_v2 = @(& git -C $repoPath status --porcelain=v2 --untracked-files=all)
}

$dockerCLI = Invoke-ObservedCommand "docker" @("version", "--format", "{{.Client.Version}}")
$dockerEngine = Invoke-ObservedCommand "docker" @("info", "--format", "{{.OSType}}/{{.Architecture}}")
$dockerDesktopVersion = ""
foreach ($registryPath in @("HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\Docker Desktop", "HKLM:\SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall\Docker Desktop")) {
    try { $dockerDesktopVersion = [string](Get-ItemProperty -LiteralPath $registryPath -Name DisplayVersion -ErrorAction Stop).DisplayVersion; break } catch { }
}
$engineParts = @($dockerEngine.output -split '/', 2)
$docker = [ordered]@{
    installed = (-not [string]::IsNullOrWhiteSpace($dockerDesktopVersion))
    desktop_version = $dockerDesktopVersion.Trim()
    cli_version = $dockerCLI.output
    engine_os = $(if ($dockerEngine.exit_code -eq 0 -and $engineParts.Count -eq 2) { $engineParts[0] } else { "" })
    engine_arch = $(if ($dockerEngine.exit_code -eq 0 -and $engineParts.Count -eq 2) { $engineParts[1] } else { "" })
}
$codex = Invoke-ObservedCommand "codex" @("--version")
$credentialPath = Join-Path $WorkflowHome "state\credentials\github.pat"
if ([string]::IsNullOrWhiteSpace($env:USERPROFILE)) { throw "USERPROFILE is required to resolve Codex user skills" }
$codexSkillsRoot = Join-Path $env:USERPROFILE ".agents\skills"
$workflowBin = Join-Path $WorkflowHome "bin"
$currentUserPath = ""
try { $currentUserPath = [string](Get-ItemProperty -LiteralPath "HKCU:\Environment" -Name Path -ErrorAction Stop).Path } catch { }
$controlPlane = [ordered]@{ state = "stopped"; diagnostic = "installed Workflow CLI is unavailable" }
$installedWorkflow = Join-Path $workflowBin "workflow.exe"
$workflow = [ordered]@{ installed = $false; owned = $false; version = ""; sha256 = ""; path_reconciled = (@($currentUserPath -split ';' | Where-Object { -not [string]::IsNullOrWhiteSpace($_) -and [string]::Equals(([IO.Path]::GetFullPath($_.Trim())), ([IO.Path]::GetFullPath($workflowBin)), [StringComparison]::OrdinalIgnoreCase) }).Count -eq 1) }
$platform = [ordered]@{ installation_recorded = $false; version = ""; release_manifest_digest = ""; platform_setup_contract_digest = "" }
$githubCredential = [ordered]@{ path = $credentialPath; exists = (Test-Path -LiteralPath $credentialPath -PathType Leaf); verified = $false; owner = ""; scopes = @() }
if (Test-Path -LiteralPath $installedWorkflow -PathType Leaf) {
    $installedVersion = Invoke-ObservedCommand $installedWorkflow @("--version")
    $versionMatch = [regex]::Match([string]$installedVersion.output, '(?<!\d)(\d+\.\d+\.\d+)(?!\d)')
    $workflow.installed = $true
    $workflow.version = $(if ($versionMatch.Success) { $versionMatch.Groups[1].Value } else { [string]$installedVersion.output })
    $workflow.sha256 = Get-SHA256File $installedWorkflow
    $inspection = Invoke-ObservedCommand $installedWorkflow @("setup", "inspect-platform", "--workflow-home", $WorkflowHome)
    if ($inspection.exit_code -eq 0) {
        try {
            $inspectionJSON = $inspection.output | ConvertFrom-Json
            if ($null -ne $inspectionJSON.result) {
                $platform = $inspectionJSON.result.platform
                $workflow.owned = [bool]$inspectionJSON.result.workflow_cli.verified
                $githubCredential.exists = [bool]$inspectionJSON.result.github_credential.exists
                $githubCredential.verified = [bool]$inspectionJSON.result.github_credential.verified
                $githubCredential.owner = [string]$inspectionJSON.result.github_credential.owner
                $githubCredential.scopes = @($inspectionJSON.result.github_credential.scopes | ForEach-Object { [string]$_ })
            }
        } catch { }
    }
    $status = Invoke-ObservedCommand $installedWorkflow @("status", "--workflow-home", $WorkflowHome)
    if ($status.exit_code -eq 0) {
        try {
            $statusJSON = $status.output | ConvertFrom-Json
            $controlPlane = [ordered]@{ state = [string]$statusJSON.state; diagnostic = [string]$statusJSON.diagnostic; runtime = $statusJSON.runtime }
        } catch {
            $controlPlane = [ordered]@{ state = "mismatched"; diagnostic = "installed Workflow CLI returned invalid status JSON" }
        }
    } else {
        $controlPlane = [ordered]@{ state = "stopped"; diagnostic = "installed Workflow CLI status command failed" }
    }
}

[ordered]@{
    schema_version = 1
    observed_at = [DateTime]::UtcNow.ToString("o")
    supported_host = ($env:OS -eq "Windows_NT")
    repository = $repoPath
    workflow_home = $WorkflowHome
    git = $gitFacts
    docker = $docker
    codex = $codex
    codex_skills_root = $codexSkillsRoot
    workflow = $workflow
    platform = $platform
    control_plane = $controlPlane
    github_credential = $githubCredential
} | ConvertTo-Json -Depth 8
