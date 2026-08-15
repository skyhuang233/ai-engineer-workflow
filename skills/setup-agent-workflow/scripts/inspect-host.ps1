[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$Repository,
    [string]$WorkflowHome = "",
    [switch]$GitProbeOnly,
    [ValidateRange(1, 120)]
    [int]$ExternalCommandTimeoutSeconds = 15
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
$windowsIdentity = [Security.Principal.WindowsIdentity]::GetCurrent()
if ($null -eq $windowsIdentity -or $null -eq $windowsIdentity.User -or [string]::IsNullOrWhiteSpace([string]$windowsIdentity.Name)) {
    throw "Current Windows identity is unavailable"
}
$currentUserSID = [string]$windowsIdentity.User.Value
$workflowHomeOwnerSID = $currentUserSID
if (Test-Path -LiteralPath $WorkflowHome) {
    $ownerAccount = New-Object Security.Principal.NTAccount((Get-Acl -LiteralPath $WorkflowHome).Owner)
    $workflowHomeOwnerSID = [string]$ownerAccount.Translate([Security.Principal.SecurityIdentifier]).Value
    if (-not [string]::Equals($workflowHomeOwnerSID, $currentUserSID, [StringComparison]::OrdinalIgnoreCase)) {
        throw "The existing Workflow Home must be owned by the current Windows user"
    }
}
$hostIdentity = [ordered]@{ user_id = $currentUserSID; username = [string]$windowsIdentity.Name; workflow_home_owner_id = $workflowHomeOwnerSID }

function ConvertTo-NativeArgument([string]$Value) {
    if ($Value.Length -gt 0 -and $Value -notmatch '[\s"]') { return $Value }
    $builder = New-Object Text.StringBuilder
    [void]$builder.Append('"')
    $slashes = 0
    foreach ($character in $Value.ToCharArray()) {
        if ($character -eq '\') { $slashes++; continue }
        if ($character -eq '"') {
            [void]$builder.Append(('\' * (2 * $slashes + 1)))
            [void]$builder.Append('"')
        } else {
            if ($slashes -gt 0) { [void]$builder.Append(('\' * $slashes)) }
            [void]$builder.Append($character)
        }
        $slashes = 0
    }
    if ($slashes -gt 0) { [void]$builder.Append(('\' * (2 * $slashes))) }
    [void]$builder.Append('"')
    return $builder.ToString()
}

function Invoke-BoundedExecutable([string]$FilePath, [string[]]$Arguments, [bool]$IsolateGit) {
    $start = New-Object Diagnostics.ProcessStartInfo
    $start.FileName = $FilePath
    $start.Arguments = (($Arguments | ForEach-Object { ConvertTo-NativeArgument ([string]$_) }) -join ' ')
    $start.UseShellExecute = $false
    $start.CreateNoWindow = $true
    $start.RedirectStandardOutput = $true
    $start.RedirectStandardError = $true
    if ($IsolateGit) {
        foreach ($name in @($start.EnvironmentVariables.Keys | ForEach-Object { [string]$_ })) {
            if ($name -match '^GIT_') { $start.EnvironmentVariables.Remove($name) }
        }
        $start.EnvironmentVariables['GIT_CONFIG_NOSYSTEM'] = '1'
        $start.EnvironmentVariables['GIT_CONFIG_GLOBAL'] = 'NUL'
        $start.EnvironmentVariables['GIT_ATTR_NOSYSTEM'] = '1'
        $start.EnvironmentVariables['GIT_OPTIONAL_LOCKS'] = '0'
        $start.EnvironmentVariables['GIT_TERMINAL_PROMPT'] = '0'
    }
    $process = New-Object Diagnostics.Process
    $process.StartInfo = $start
    try {
        if (-not $process.Start()) { throw "failed to start external command" }
        $stdout = $process.StandardOutput.ReadToEndAsync()
        $stderr = $process.StandardError.ReadToEndAsync()
        if (-not $process.WaitForExit($ExternalCommandTimeoutSeconds * 1000)) {
            try { $process.Kill() } catch { }
            throw "external command exceeded the $ExternalCommandTimeoutSeconds second inspection timeout"
        }
        $process.WaitForExit()
        return [ordered]@{ output = [string]$stdout.Result; error = [string]$stderr.Result; exit_code = [int]$process.ExitCode }
    } finally {
        $process.Dispose()
    }
}

function Invoke-ObservedCommand([string]$Name, [string[]]$Arguments) {
    $command = Get-Command $Name -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($null -eq $command) {
        return [ordered]@{ installed = $false; output = ""; exit_code = $null }
    }
    $result = Invoke-BoundedExecutable $command.Source $Arguments $false
    $combined = ([string]$result.output + [string]$result.error).Trim()
    return [ordered]@{ installed = $true; output = $combined; exit_code = $result.exit_code }
}

function Invoke-IsolatedGit([string[]]$Arguments) {
    $gitCommand = Get-Command "git" -CommandType Application -ErrorAction Stop | Select-Object -First 1
    return Invoke-BoundedExecutable $gitCommand.Source $Arguments $true
}

function Test-UnsafeRepositoryGitKey([string]$Key) {
    $keyLower = $Key.Trim().ToLowerInvariant()
    return $keyLower.StartsWith('http.') -or $keyLower.StartsWith('https.') -or
        $keyLower.StartsWith('credential.') -or $keyLower.StartsWith('include.') -or $keyLower.StartsWith('includeif.') -or
        $keyLower.StartsWith('protocol.') -or $keyLower.StartsWith('filter.') -or
        ($keyLower.StartsWith('diff.') -and ($keyLower.EndsWith('.textconv') -or $keyLower.EndsWith('.external') -or $keyLower.EndsWith('.command'))) -or
        ($keyLower.StartsWith('merge.') -and $keyLower.EndsWith('.driver')) -or
        ($keyLower.StartsWith('url.') -and ($keyLower.EndsWith('.insteadof') -or $keyLower.EndsWith('.pushinsteadof'))) -or
        ($keyLower.StartsWith('remote.') -and ($keyLower.EndsWith('.vcs') -or $keyLower.EndsWith('.proxy') -or $keyLower.EndsWith('.uploadpack') -or $keyLower.EndsWith('.receivepack'))) -or
        @('core.gitproxy', 'core.sshcommand', 'core.fsmonitor', 'core.attributesfile', 'core.hookspath') -contains $keyLower
}

function Read-GitConfigNames([string]$Path, [string]$Scope) {
    $result = Invoke-IsolatedGit @('-C', $Path, 'config', $Scope, '--no-includes', '-z', '--name-only', '--get-regexp', '.*')
    if ($result.exit_code -eq 1) { return @() }
    if ($result.exit_code -ne 0) { throw "Unable to inspect $Scope Git configuration: $($result.error.Trim())" }
    return @(([string]$result.output -split "`0") | Where-Object { -not [string]::IsNullOrWhiteSpace($_) })
}

function Assert-SafeRepositoryGitConfiguration([string]$Path) {
    $localNames = @(Read-GitConfigNames $Path '--local')
    foreach ($name in $localNames) {
        if (Test-UnsafeRepositoryGitKey $name) { throw "unsafe repository-local Git configuration: '$name'" }
    }
    $worktreeEnabled = Invoke-IsolatedGit @('-C', $Path, 'config', '--local', '--no-includes', '--get', 'extensions.worktreeConfig')
    if ($worktreeEnabled.exit_code -eq 0 -and ([string]$worktreeEnabled.output).Trim() -eq 'true') {
        foreach ($name in @(Read-GitConfigNames $Path '--worktree')) {
            if (Test-UnsafeRepositoryGitKey $name) { throw "unsafe repository-local Git configuration: '$name'" }
        }
    } elseif ($worktreeEnabled.exit_code -ne 1) {
        throw "Unable to inspect repository worktree Git configuration: $($worktreeEnabled.error.Trim())"
    }
}

function Read-RawLocalOrigin([string]$Path) {
    $result = Invoke-IsolatedGit @('-C', $Path, 'config', '--local', '--no-includes', '--get-all', 'remote.origin.url')
    $values = @(([string]$result.output -split "`r?`n") | Where-Object { $_ -ne '' })
    if ($result.exit_code -eq 1 -and $values.Count -eq 0) { return "" }
    if ($result.exit_code -ne 0) { throw "Unable to read local remote.origin.url exactly: $($result.error.Trim())" }
    if ($values.Count -ne 1 -or [string]::IsNullOrWhiteSpace($values[0])) {
        throw "A Git repository must have zero or exactly one local remote.origin.url"
    }
    return $values[0]
}

$gitCommand = Get-Command "git" -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
$git = $(if ($null -eq $gitCommand) { [ordered]@{ installed = $false; output = ""; exit_code = $null } } else {
    $result = Invoke-IsolatedGit @('-C', $repoPath, 'rev-parse', '--show-toplevel')
    [ordered]@{ installed = $true; output = ([string]$result.output).Trim(); exit_code = $result.exit_code }
})
$isRepository = $git.installed -and $git.exit_code -eq 0
$gitFacts = [ordered]@{ installed = $git.installed; is_repository = $isRepository }
if ($isRepository) {
    Assert-SafeRepositoryGitConfiguration $repoPath
    $gitFacts.root = $git.output
    $branchResult = Invoke-IsolatedGit @('-C', $repoPath, 'branch', '--show-current')
    $headResult = Invoke-IsolatedGit @('-C', $repoPath, 'rev-parse', '--verify', 'HEAD')
    if ($branchResult.exit_code -ne 0 -or $headResult.exit_code -ne 0) { throw "Unable to inspect repository branch and HEAD" }
    $gitFacts.branch = ([string]$branchResult.output).Trim()
    $gitFacts.head = ([string]$headResult.output).Trim()
    $gitFacts.origin = Read-RawLocalOrigin $repoPath
    $statusResult = Invoke-IsolatedGit @('-C', $repoPath, 'status', '--porcelain=v2', '-z', '--untracked-files=all')
    if ($statusResult.exit_code -ne 0) { throw "Unable to inspect repository status: $($statusResult.error.Trim())" }
    $gitFacts.status_porcelain_v2 = @([string]$statusResult.output)
}
if ($GitProbeOnly) { $gitFacts | ConvertTo-Json -Depth 4; exit 0 }

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
$workflow = [ordered]@{ installed = $false; owned = $false; version = ""; sha256 = ""; trust_state = "absent"; diagnostic = "installed Workflow CLI is absent"; path_reconciled = (@($currentUserPath -split ';' | Where-Object { -not [string]::IsNullOrWhiteSpace($_) -and [string]::Equals(([IO.Path]::GetFullPath($_.Trim())), ([IO.Path]::GetFullPath($workflowBin)), [StringComparison]::OrdinalIgnoreCase) }).Count -eq 1) }
$platform = [ordered]@{ installation_recorded = $false; version = ""; release_manifest_digest = ""; platform_setup_contract_digest = "" }
$bootstrapPinPath = Join-Path $WorkflowHome "config\bootstrap-platform-release-pin.json"
$bootstrapPinBackupPath = Join-Path $WorkflowHome "backups\bootstrap-platform-release-pin.json"
$bootstrapPinLoaded = $false
$bootstrapPinConflictDiagnostic = ""
$bootstrapPinRepairDiagnostic = ""
function Read-VerifiedBootstrapPin([string]$Path, [string]$Description) {
    $pinScratch = Join-Path ([IO.Path]::GetTempPath()) ("workflow-bootstrap-pin-" + [Guid]::NewGuid().ToString("N"))
    New-Item -ItemType Directory -Path $pinScratch | Out-Null
    try {
        $pinRaw = Get-Content -LiteralPath $Path -Raw
        $pin = $pinRaw | ConvertFrom-Json
        $pinPropertyNames = @($pin.PSObject.Properties.Name | Sort-Object)
        $expectedPinPropertyNames = @("schema_version", "release_version", "release_manifest_digest_sha256", "platform_setup_contract_digest_sha256", "workflow_cli_sha256", "release_bundled_files_json", "release_bundled_files_digest_sha256", "control_plane_plan_digest_sha256", "manifest_base64", "signature_base64" | Sort-Object)
        if (($pinPropertyNames -join "`n") -cne ($expectedPinPropertyNames -join "`n")) { throw "$Description contains missing or unknown fields" }
        if ([int]$pin.schema_version -ne 1 -or ([string]$pin.release_manifest_digest_sha256) -notmatch '^[0-9a-f]{64}$' -or ([string]$pin.platform_setup_contract_digest_sha256) -notmatch '^[0-9a-f]{64}$' -or ([string]$pin.workflow_cli_sha256) -notmatch '^[0-9a-f]{64}$' -or ([string]$pin.release_bundled_files_digest_sha256) -notmatch '^[0-9a-f]{64}$' -or ([string]$pin.control_plane_plan_digest_sha256) -notmatch '^[0-9a-f]{64}$' -or [string]::IsNullOrWhiteSpace([string]$pin.release_version)) { throw "$Description has invalid metadata" }
        $pinnedManifestPath = Join-Path $pinScratch "platform-release.json"
        $pinnedSignaturePath = Join-Path $pinScratch "platform-release.json.sig"
        [IO.File]::WriteAllBytes($pinnedManifestPath, [Convert]::FromBase64String([string]$pin.manifest_base64))
        [IO.File]::WriteAllBytes($pinnedSignaturePath, [Convert]::FromBase64String([string]$pin.signature_base64))
        & (Join-Path $PSScriptRoot "verify-platform-release.ps1") -ManifestPath $pinnedManifestPath -SignaturePath $pinnedSignaturePath | Out-Null
        $pinnedManifest = Get-Content -LiteralPath $pinnedManifestPath -Raw | ConvertFrom-Json
        $actualPinnedDigest = Get-SHA256File $pinnedManifestPath
        $contractInput = Join-Path $pinScratch "platform-contract.input.json"
        $contractCanonical = Join-Path $pinScratch "platform-contract.canonical.json"
        [IO.File]::WriteAllText($contractInput, ($pinnedManifest.platform_setup_contract | ConvertTo-Json -Depth 20 -Compress), (New-Object Text.UTF8Encoding($false)))
        $actualContractDigest = (& (Join-Path $PSScriptRoot "convert-to-setup-canonical-json.ps1") -InputPath $contractInput -OutputPath $contractCanonical | Select-Object -Last 1).Trim()
        $bundledFilesInput = Join-Path $pinScratch "bundled-files.input.json"
        $bundledFilesCanonical = Join-Path $pinScratch "bundled-files.canonical.json"
        [IO.File]::WriteAllText($bundledFilesInput, (ConvertTo-Json -InputObject @($pinnedManifest.bundled_files) -Depth 10 -Compress), (New-Object Text.UTF8Encoding($false)))
        $actualBundledFilesDigest = (& (Join-Path $PSScriptRoot "convert-to-setup-canonical-json.ps1") -InputPath $bundledFilesInput -OutputPath $bundledFilesCanonical | Select-Object -Last 1).Trim()
        $actualBundledFilesJSON = [IO.File]::ReadAllText($bundledFilesCanonical, (New-Object Text.UTF8Encoding($false, $true)))
        $workflowPins = @($pinnedManifest.bundled_files | Where-Object { [string]$_.path -eq "bin/workflow.exe" })
        if ($actualPinnedDigest -cne [string]$pin.release_manifest_digest_sha256 -or [string]$pinnedManifest.release.version -cne [string]$pin.release_version -or $actualContractDigest -cne [string]$pin.platform_setup_contract_digest_sha256 -or $actualBundledFilesDigest -cne [string]$pin.release_bundled_files_digest_sha256 -or $actualBundledFilesJSON -cne [string]$pin.release_bundled_files_json -or $workflowPins.Count -ne 1 -or [string]$workflowPins[0].sha256 -cne [string]$pin.workflow_cli_sha256) { throw "$Description identity differs from its verified manifest" }
        return [pscustomobject]@{
            raw = $pinRaw
            platform = [ordered]@{ installation_recorded = $true; version = [string]$pin.release_version; release_manifest_digest = $actualPinnedDigest; platform_setup_contract_digest = $actualContractDigest; workflow_cli_sha256 = [string]$pin.workflow_cli_sha256; release_bundled_files_json = $actualBundledFilesJSON; release_bundled_files_digest = $actualBundledFilesDigest; control_plane_plan_digest_sha256 = [string]$pin.control_plane_plan_digest_sha256 }
        }
    } finally {
        Remove-Item -LiteralPath $pinScratch -Recurse -Force -ErrorAction SilentlyContinue
    }
}
$primaryPin = $null
$backupPin = $null
$primaryPinError = ""
$backupPinError = ""
if (Test-Path -LiteralPath $bootstrapPinPath -PathType Leaf) {
    try { $primaryPin = Read-VerifiedBootstrapPin $bootstrapPinPath "primary Bootstrap Platform Release pin" } catch { $primaryPinError = $_.Exception.Message }
}
if (Test-Path -LiteralPath $bootstrapPinBackupPath -PathType Leaf) {
    try { $backupPin = Read-VerifiedBootstrapPin $bootstrapPinBackupPath "backup Bootstrap Platform Release pin" } catch { $backupPinError = $_.Exception.Message }
}
if ($null -ne $primaryPin -and $null -ne $backupPin) {
    if ([string]$primaryPin.raw -cne [string]$backupPin.raw) {
        $bootstrapPinConflictDiagnostic = "Bootstrap Platform Release pin conflict: verified primary and backup pins differ"
    } else {
        $platform = $primaryPin.platform
        $bootstrapPinLoaded = $true
    }
} elseif ($null -ne $backupPin) {
    $platform = $backupPin.platform
    $bootstrapPinLoaded = $true
    $bootstrapPinRepairDiagnostic = "Bootstrap Platform Release primary pin requires repair; exact release authority was recovered from the verified read-only backup"
} elseif ($null -ne $primaryPin) {
    $platform = $primaryPin.platform
    $bootstrapPinLoaded = $true
    $bootstrapPinRepairDiagnostic = "Bootstrap Platform Release backup pin requires repair from the verified primary"
} elseif (-not [string]::IsNullOrWhiteSpace($primaryPinError) -or -not [string]::IsNullOrWhiteSpace($backupPinError)) {
    $pinErrors = @($primaryPinError, $backupPinError) | Where-Object { -not [string]::IsNullOrWhiteSpace($_) }
    $bootstrapPinConflictDiagnostic = "Bootstrap Platform Release pin conflict: $pinErrors"
}
if (-not [string]::IsNullOrWhiteSpace($bootstrapPinConflictDiagnostic)) {
    $workflow.trust_state = "conflict"
    $workflow.diagnostic = $bootstrapPinConflictDiagnostic
}
$githubCredential = [ordered]@{ path = $credentialPath; exists = (Test-Path -LiteralPath $credentialPath -PathType Leaf); verified = $false; login = ""; owner = ""; scopes = @(); fingerprint_sha256 = "" }
$codexAuth = [ordered]@{ verified = $false; source = ""; fingerprint_sha256 = "" }
if ($codex.installed) {
    $doctor = Invoke-ObservedCommand "codex" @("doctor", "--json")
    $loginStatus = Invoke-ObservedCommand "codex" @("login", "status")
    try {
        # Codex doctor may exit nonzero because of unrelated terminal checks;
        # the two required redacted checks remain authoritative when valid.
        $report = $doctor.output | ConvertFrom-Json
        $authCheck = $report.checks."auth.credentials"
        $configCheck = $report.checks."config.load"
        if ([int]$report.schemaVersion -ne 1 -or [string]::IsNullOrWhiteSpace([string]$report.codexVersion) -or [string]$authCheck.status -ne "ok" -or [string]$configCheck.status -ne "ok") { throw "unsupported Codex doctor report" }
        if ([string]$authCheck.details."stored ChatGPT tokens" -ne "true" -or -not [string]::Equals([string]$authCheck.details."stored auth mode", "chatgpt", [StringComparison]::OrdinalIgnoreCase)) { throw "Codex doctor did not verify ChatGPT tokens" }
        $discoveredSource = [string]$authCheck.details."auth file"
        $reportedHome = [string]$configCheck.details.CODEX_HOME
        if (-not [IO.Path]::IsPathRooted($discoveredSource) -or -not [IO.Path]::IsPathRooted($reportedHome) -or -not [string]::Equals([IO.Path]::GetFullPath((Split-Path -Parent $discoveredSource)), [IO.Path]::GetFullPath($reportedHome), [StringComparison]::OrdinalIgnoreCase)) { throw "Codex doctor returned an invalid authentication source boundary" }
        $source = $(if ([string]::IsNullOrWhiteSpace($env:WORKFLOW_CODEX_AUTH_FILE)) { $discoveredSource } else { $env:WORKFLOW_CODEX_AUTH_FILE })
        if (-not [IO.Path]::IsPathRooted($source) -or -not (Test-Path -LiteralPath $source -PathType Leaf)) { throw "Codex authentication source is unavailable" }
        if ($loginStatus.exit_code -ne 0 -or -not ([string]$loginStatus.output).ToLowerInvariant().Contains("logged in using chatgpt")) { throw "Codex login status is not ChatGPT" }
        $cache = [IO.File]::ReadAllText($source) | ConvertFrom-Json
        if ([string]$cache.auth_mode -ne "chatgpt" -or [string]::IsNullOrWhiteSpace([string]$cache.tokens.access_token) -or [string]::IsNullOrWhiteSpace([string]$cache.tokens.account_id) -or [string]::IsNullOrWhiteSpace([string]$cache.tokens.id_token) -or [string]::IsNullOrWhiteSpace([string]$cache.tokens.refresh_token)) { throw "Codex authentication cache is invalid" }
        $codexAuth.verified = $true
        $codexAuth.source = [IO.Path]::GetFullPath($source)
        $codexAuth.fingerprint_sha256 = Get-SHA256File $source
    } catch { }
}
if (Test-Path -LiteralPath $installedWorkflow -PathType Leaf) {
    $workflow.installed = $true
    $workflow.sha256 = Get-SHA256File $installedWorkflow
    $workflowTrustedForExecution = $false
    if (-not [string]::IsNullOrWhiteSpace($bootstrapPinConflictDiagnostic)) {
        $workflow.trust_state = "conflict"
        $workflow.diagnostic = $bootstrapPinConflictDiagnostic
    } elseif (-not [string]::IsNullOrWhiteSpace($bootstrapPinRepairDiagnostic)) {
        $workflow.trust_state = "repair_required"
        $workflow.diagnostic = $bootstrapPinRepairDiagnostic
    } elseif (-not $bootstrapPinLoaded) {
        $workflow.trust_state = "repair_required"
        $workflow.diagnostic = "Bootstrap Platform Release primary and backup pins are missing; recover the exact installed version before repairing the Workflow CLI"
    } elseif (-not [string]::Equals([IO.Path]::GetFullPath($installedWorkflow), [IO.Path]::GetFullPath((Join-Path $WorkflowHome "bin\workflow.exe")), [StringComparison]::OrdinalIgnoreCase)) {
        $workflow.trust_state = "conflict"
        $workflow.diagnostic = "Installed Workflow CLI path differs from the fixed Workflow Home path"
    } elseif ($workflow.sha256 -cne [string]$platform.workflow_cli_sha256) {
        $workflow.trust_state = "repair_required"
        $workflow.diagnostic = "Installed Workflow CLI SHA-256 differs from the verified Bootstrap Platform Release pin"
    } else {
        try {
            $workflowItem = Get-Item -LiteralPath $installedWorkflow -Force
            if (($workflowItem.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0 -or -not [string]::Equals([IO.Path]::GetFullPath($workflowItem.FullName), [IO.Path]::GetFullPath($installedWorkflow), [StringComparison]::OrdinalIgnoreCase)) {
                throw "Installed Workflow CLI path is a reparse point or resolves outside the fixed Workflow Home path"
            }
            $workflowOwnerAccount = New-Object Security.Principal.NTAccount((Get-Acl -LiteralPath $installedWorkflow).Owner)
            $workflowOwnerSID = [string]$workflowOwnerAccount.Translate([Security.Principal.SecurityIdentifier]).Value
            if (-not [string]::Equals($workflowOwnerSID, $currentUserSID, [StringComparison]::OrdinalIgnoreCase)) {
                throw "Installed Workflow CLI is not owned by the current Windows user"
            }
            $workflowTrustedForExecution = $true
            $workflow.trust_state = "pinned"
            $workflow.diagnostic = "Installed Workflow CLI fixed path, current-user ownership, and SHA-256 match the verified Bootstrap Platform Release pin"
        } catch {
            $workflow.trust_state = "conflict"
            $workflow.diagnostic = $_.Exception.Message
        }
    }
    if ($workflowTrustedForExecution) {
        $installedVersion = Invoke-ObservedCommand $installedWorkflow @("--version")
        $versionMatch = [regex]::Match([string]$installedVersion.output, '(?<!\d)(\d+\.\d+\.\d+)(?!\d)')
        $workflow.version = $(if ($versionMatch.Success) { $versionMatch.Groups[1].Value } else { [string]$installedVersion.output })
        $inspection = Invoke-ObservedCommand $installedWorkflow @("setup", "inspect-platform", "--workflow-home", $WorkflowHome)
        if ($inspection.exit_code -eq 0) {
            try {
                $inspectionJSON = $inspection.output | ConvertFrom-Json
                if ($null -ne $inspectionJSON.result) {
                    $inspectedPlatform = $inspectionJSON.result.platform
                    if (-not [bool]$inspectedPlatform.installation_recorded -or [string]$inspectedPlatform.version -cne [string]$platform.version -or [string]$inspectedPlatform.release_manifest_digest -cne [string]$platform.release_manifest_digest) { throw "installed Workflow CLI state differs from the verified Bootstrap Platform Release pin" }
                    $platform = $inspectedPlatform
                    $workflow.owned = [bool]$inspectionJSON.result.workflow_cli.verified
                    $githubCredential.exists = [bool]$inspectionJSON.result.github_credential.exists
                    $githubCredential.verified = [bool]$inspectionJSON.result.github_credential.verified
                    $githubCredential.login = [string]$inspectionJSON.result.github_credential.login
                    $githubCredential.owner = [string]$inspectionJSON.result.github_credential.owner
                    $githubCredential.scopes = @($inspectionJSON.result.github_credential.scopes | ForEach-Object { [string]$_ })
					$githubCredential.fingerprint_sha256 = [string]$inspectionJSON.result.github_credential.fingerprint_sha256
					$codexAuth.verified = [bool]$inspectionJSON.result.codex_auth.verified
					$codexAuth.source = [string]$inspectionJSON.result.codex_auth.source
					$codexAuth.fingerprint_sha256 = [string]$inspectionJSON.result.codex_auth.fingerprint_sha256
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
}

[ordered]@{
    schema_version = 1
    observed_at = [DateTime]::UtcNow.ToString("o")
    supported_host = ($env:OS -eq "Windows_NT")
    repository = $repoPath
    workflow_home = $WorkflowHome
	host_identity = $hostIdentity
    git = $gitFacts
    docker = $docker
    codex = $codex
	codex_auth = $codexAuth
    codex_skills_root = $codexSkillsRoot
    workflow = $workflow
    platform = $platform
    control_plane = $controlPlane
    github_credential = $githubCredential
} | ConvertTo-Json -Depth 8
