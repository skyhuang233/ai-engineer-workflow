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
    $previousOptionalLocks = $env:GIT_OPTIONAL_LOCKS
    try {
        $env:GIT_OPTIONAL_LOCKS = "0"
        $gitFacts.status_porcelain_v2 = @(& git -C $repoPath status --porcelain=v2 -z --untracked-files=all)
    } finally {
        $env:GIT_OPTIONAL_LOCKS = $previousOptionalLocks
    }
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
$workflow = [ordered]@{ installed = $false; owned = $false; version = ""; sha256 = ""; trust_state = "absent"; diagnostic = "installed Workflow CLI is absent"; path_reconciled = (@($currentUserPath -split ';' | Where-Object { -not [string]::IsNullOrWhiteSpace($_) -and [string]::Equals(([IO.Path]::GetFullPath($_.Trim())), ([IO.Path]::GetFullPath($workflowBin)), [StringComparison]::OrdinalIgnoreCase) }).Count -eq 1) }
$platform = [ordered]@{ installation_recorded = $false; version = ""; release_manifest_digest = ""; platform_setup_contract_digest = "" }
$bootstrapPinPath = Join-Path $WorkflowHome "config\bootstrap-platform-release-pin.json"
$bootstrapPinLoaded = $false
$bootstrapPinConflictDiagnostic = ""
if (Test-Path -LiteralPath $bootstrapPinPath -PathType Leaf) {
    $pinScratch = Join-Path ([IO.Path]::GetTempPath()) ("workflow-bootstrap-pin-" + [Guid]::NewGuid().ToString("N"))
    New-Item -ItemType Directory -Path $pinScratch | Out-Null
    try {
        $pin = Get-Content -LiteralPath $bootstrapPinPath -Raw | ConvertFrom-Json
        $pinPropertyNames = @($pin.PSObject.Properties.Name | Sort-Object)
        $expectedPinPropertyNames = @("schema_version", "release_version", "release_manifest_digest_sha256", "platform_setup_contract_digest_sha256", "workflow_cli_sha256", "release_bundled_files_json", "release_bundled_files_digest_sha256", "control_plane_plan_digest_sha256", "manifest_base64", "signature_base64" | Sort-Object)
        if (($pinPropertyNames -join "`n") -cne ($expectedPinPropertyNames -join "`n")) { throw "pin contains missing or unknown fields" }
        if ([int]$pin.schema_version -ne 1 -or ([string]$pin.release_manifest_digest_sha256) -notmatch '^[0-9a-f]{64}$' -or ([string]$pin.platform_setup_contract_digest_sha256) -notmatch '^[0-9a-f]{64}$' -or ([string]$pin.workflow_cli_sha256) -notmatch '^[0-9a-f]{64}$' -or ([string]$pin.release_bundled_files_digest_sha256) -notmatch '^[0-9a-f]{64}$' -or ([string]$pin.control_plane_plan_digest_sha256) -notmatch '^[0-9a-f]{64}$' -or [string]::IsNullOrWhiteSpace([string]$pin.release_version)) { throw "invalid pin metadata" }
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
        if ($actualPinnedDigest -cne [string]$pin.release_manifest_digest_sha256 -or [string]$pinnedManifest.release.version -cne [string]$pin.release_version -or $actualContractDigest -cne [string]$pin.platform_setup_contract_digest_sha256 -or $actualBundledFilesDigest -cne [string]$pin.release_bundled_files_digest_sha256 -or $actualBundledFilesJSON -cne [string]$pin.release_bundled_files_json -or $workflowPins.Count -ne 1 -or [string]$workflowPins[0].sha256 -cne [string]$pin.workflow_cli_sha256) { throw "pin identity differs from its verified manifest" }
        $platform = [ordered]@{ installation_recorded = $true; version = [string]$pin.release_version; release_manifest_digest = $actualPinnedDigest; platform_setup_contract_digest = $actualContractDigest; workflow_cli_sha256 = [string]$pin.workflow_cli_sha256; release_bundled_files_json = $actualBundledFilesJSON; release_bundled_files_digest = $actualBundledFilesDigest; control_plane_plan_digest_sha256 = [string]$pin.control_plane_plan_digest_sha256 }
        $bootstrapPinLoaded = $true
    } catch {
        $bootstrapPinConflictDiagnostic = "Bootstrap Platform Release pin conflict: $($_.Exception.Message)"
    } finally {
        Remove-Item -LiteralPath $pinScratch -Recurse -Force -ErrorAction SilentlyContinue
    }
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
    } elseif (-not $bootstrapPinLoaded) {
        $workflow.trust_state = "repair_required"
        $workflow.diagnostic = "Bootstrap Platform Release pin is missing; repair the installed Workflow CLI before execution"
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
