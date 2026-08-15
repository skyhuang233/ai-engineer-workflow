[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$ManifestPath,
    [Parameter(Mandatory = $true)][string]$SignaturePath,
    [Parameter(Mandatory = $true)][string]$HostFactsPath,
    [Parameter(Mandatory = $true)][string]$OutputPath,
    [Parameter(Mandatory = $true)][string]$GitHubOwner,
    [switch]$AllowUpgrade,
    [string]$PolicyPath = (Join-Path $PSScriptRoot "..\trust\release-policy.json"),
    [string]$PublicKeyPath = ""
)

$ErrorActionPreference = "Stop"
function Get-SHA256Hex([byte[]]$Bytes) {
    $hasher = [Security.Cryptography.SHA256]::Create()
    try { return ([BitConverter]::ToString($hasher.ComputeHash($Bytes))).Replace("-", "").ToLowerInvariant() } finally { $hasher.Dispose() }
}
function Get-SHA256File([string]$Path) {
    return Get-SHA256Hex ([IO.File]::ReadAllBytes($Path))
}
$verificationArguments = @{
    ManifestPath = $ManifestPath
    SignaturePath = $SignaturePath
    PolicyPath = $PolicyPath
}
if (-not [string]::IsNullOrWhiteSpace($PublicKeyPath)) { $verificationArguments.PublicKeyPath = $PublicKeyPath }
& (Join-Path $PSScriptRoot "verify-platform-release.ps1") @verificationArguments | Out-Null

$manifest = Get-Content -LiteralPath $ManifestPath -Raw | ConvertFrom-Json
$facts = Get-Content -LiteralPath $HostFactsPath -Raw | ConvertFrom-Json
if ($manifest.schema_version -ne 1) { throw "Unsupported Platform Release Manifest schema" }
if ([int]$manifest.bootstrap_contract.minimum_schema -gt 1 -or [int]$manifest.bootstrap_contract.maximum_schema -lt 1) { throw "Platform Release is incompatible with this bootstrap planner" }
if ($facts.schema_version -ne 1) { throw "Unsupported host-facts schema" }
if (-not $facts.supported_host) { throw "Agent Workflow setup supports Windows only" }

$actions = [System.Collections.Generic.List[object]]::new()
$manifestDigest = Get-SHA256File $ManifestPath
if ($facts.platform.installation_recorded -and -not [string]::IsNullOrWhiteSpace([string]$facts.platform.release_manifest_digest) -and [string]$facts.platform.release_manifest_digest -ne $manifestDigest -and -not $AllowUpgrade) {
    throw "The installed Platform Release pin differs; reuse its exact manifest unless the user explicitly requested an upgrade"
}
$canonicalizer = Join-Path $PSScriptRoot "convert-to-setup-canonical-json.ps1"
$scratchRoot = Join-Path ([IO.Path]::GetTempPath()) ("workflow-plan-" + [Guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $scratchRoot | Out-Null
$contractInput = Join-Path $scratchRoot "contract.input.json"; $contractCanonical = Join-Path $scratchRoot "contract.canonical.json"
[IO.File]::WriteAllText($contractInput, ($manifest.platform_setup_contract | ConvertTo-Json -Depth 20 -Compress), (New-Object Text.UTF8Encoding($false)))
$platformSetupContractDigest = (& $canonicalizer -InputPath $contractInput -OutputPath $contractCanonical | Select-Object -Last 1).Trim()
$platformSetupContractJSON = [IO.File]::ReadAllText($contractCanonical, (New-Object Text.UTF8Encoding($false, $true)))
$bundledFilesInput = Join-Path $scratchRoot "bundled-files.input.json"; $bundledFilesCanonical = Join-Path $scratchRoot "bundled-files.canonical.json"
[IO.File]::WriteAllText($bundledFilesInput, (ConvertTo-Json -InputObject @($manifest.bundled_files) -Depth 10 -Compress), (New-Object Text.UTF8Encoding($false)))
$releaseBundledFilesDigest = (& $canonicalizer -InputPath $bundledFilesInput -OutputPath $bundledFilesCanonical | Select-Object -Last 1).Trim()
$releaseBundledFilesJSON = [IO.File]::ReadAllText($bundledFilesCanonical, (New-Object Text.UTF8Encoding($false, $true)))
$workflowExecutable = @($manifest.bundled_files | Where-Object { [string]$_.path -eq "bin/workflow.exe" } | Select-Object -First 1)
if ($workflowExecutable.Count -ne 1) { throw "Verified Platform Release has no exact Workflow CLI checksum" }
function Add-PlatformPins([Collections.IDictionary]$Parameters) {
    $Parameters.release_manifest_digest = $manifestDigest
    $Parameters.platform_setup_contract_digest = $platformSetupContractDigest
    $Parameters.workflow_cli_sha256 = [string]$workflowExecutable[0].sha256
    $Parameters.release_bundled_files_digest = $releaseBundledFilesDigest
    return $Parameters
}
$workflowCurrent = ($facts.workflow.installed -and $facts.workflow.owned -and $facts.workflow.path_reconciled -and [string]$facts.workflow.version -eq [string]$manifest.release.version -and [string]$facts.workflow.sha256 -eq [string]$workflowExecutable[0].sha256)
if (-not $workflowCurrent) {
    $actions.Add([ordered]@{ id = "install-platform-cli"; kind = "platform_cli"; subject = (Join-Path $facts.workflow_home "bin\workflow.exe"); action = "install"; parameters = (Add-PlatformPins ([ordered]@{ version = [string]$manifest.release.version; sha256 = [string]$workflowExecutable[0].sha256 })) })
}
$managedSkills = @($manifest.platform_setup_contract.workflow_skill_bundle.managed_skills | ForEach-Object { [string]$_ })
$skillFiles = @($manifest.bundled_files | Where-Object { ([string]$_.path).StartsWith("skills/") } | ForEach-Object {
    [ordered]@{ path = ([string]$_.path).Substring(7); sha256 = [string]$_.sha256 }
})
if ($managedSkills.Count -eq 0 -or $skillFiles.Count -eq 0) { throw "Verified Platform Release has no Workflow Skill Bundle files" }
$bundleCurrent = $true
$bundleStatePath = Join-Path $facts.workflow_home "config\workflow-skills.owner.json"
if (-not (Test-Path -LiteralPath $bundleStatePath -PathType Leaf)) {
    $bundleCurrent = $false
} else {
    $bundleState = Get-Content -LiteralPath $bundleStatePath -Raw | ConvertFrom-Json
    if ($bundleState.owner -ne "agent-workflow-platform" -or [string]$bundleState.version -ne [string]$manifest.platform_setup_contract.workflow_skill_bundle.version) { $bundleCurrent = $false }
}
foreach ($skill in $managedSkills) {
    if ($skill -ne [IO.Path]::GetFileName($skill)) { throw "Verified Platform Release has an invalid managed skill name" }
    $skillRoot = Join-Path $facts.codex_skills_root $skill
    if (-not (Test-Path -LiteralPath $skillRoot)) { $bundleCurrent = $false; continue }
    if (-not (Test-Path -LiteralPath $skillRoot -PathType Container)) { throw "Existing skill '$skill' is not owned by Agent Workflow" }
    $ownerPath = Join-Path $skillRoot ".agent-workflow-owner.json"
    if (-not (Test-Path -LiteralPath $ownerPath -PathType Leaf)) { throw "Existing skill '$skill' is not owned by Agent Workflow" }
    $owner = Get-Content -LiteralPath $ownerPath -Raw | ConvertFrom-Json
    if ($owner.owner -ne "agent-workflow-platform") { throw "Existing skill '$skill' is not owned by Agent Workflow" }
    if ([string]$owner.version -ne [string]$manifest.platform_setup_contract.workflow_skill_bundle.version) { $bundleCurrent = $false; continue }
    $expectedForSkill = @($skillFiles | Where-Object { ([string]$_.path).StartsWith("$skill/") })
    $actualFiles = @(Get-ChildItem -LiteralPath $skillRoot -Recurse -File | Where-Object { $_.FullName -ne $ownerPath })
    if ($actualFiles.Count -ne $expectedForSkill.Count) { $bundleCurrent = $false; continue }
    foreach ($file in $expectedForSkill) {
        $relativeWithinSkill = ([string]$file.path).Substring($skill.Length + 1).Replace('/', [IO.Path]::DirectorySeparatorChar)
        $path = Join-Path $skillRoot $relativeWithinSkill
        if (-not (Test-Path -LiteralPath $path -PathType Leaf)) { $bundleCurrent = $false; break }
        if ((Get-SHA256File $path) -ne [string]$file.sha256) { $bundleCurrent = $false; break }
    }
}
if (-not $bundleCurrent) {
    $managedSkillsJSON = ConvertTo-Json -InputObject ([object[]]$managedSkills) -Compress
    $skillFilesJSON = ConvertTo-Json -InputObject ([object[]]$skillFiles) -Depth 5 -Compress
    $actions.Add([ordered]@{
        id = "install-workflow-skill-bundle"
        kind = "workflow_skill_bundle"
        subject = [IO.Path]::GetFullPath([string]$facts.codex_skills_root)
        action = "install"
        parameters = (Add-PlatformPins ([ordered]@{
            version = [string]$manifest.platform_setup_contract.workflow_skill_bundle.version
            managed_skills_json = $managedSkillsJSON
            files_json = $skillFilesJSON
        }))
    })
}
$dockerVersionMatches = ($facts.docker.installed -and [string]$facts.docker.desktop_version -eq [string]$manifest.platform_setup_contract.docker_desktop.version)
$dockerEngineMatches = ([string]$facts.docker.engine_os -eq "linux" -and @("amd64", "x86_64") -contains [string]$facts.docker.engine_arch)
if (-not $dockerVersionMatches -or -not $dockerEngineMatches) {
    $dockerAction = $(if (-not $facts.docker.installed) { "install" } elseif (-not $dockerVersionMatches) { "upgrade" } else { "repair" })
    $actions.Add([ordered]@{ id = "install-docker-desktop"; kind = "docker_desktop"; subject = "current-host"; action = $dockerAction; parameters = (Add-PlatformPins ([ordered]@{ version = [string]$manifest.platform_setup_contract.docker_desktop.version; installer_url = [string]$manifest.platform_setup_contract.docker_desktop.installer_url; windows_amd64_sha256 = [string]$manifest.platform_setup_contract.docker_desktop.windows_amd64_sha256 })) })
}
$observedScopes = @($facts.github_credential.scopes | ForEach-Object { [string]$_ })
$effectiveGitHubOwner = $GitHubOwner
$effectiveGitHubOwner = $effectiveGitHubOwner.Trim()
if ([string]::IsNullOrWhiteSpace($effectiveGitHubOwner)) { throw "GitHubOwner must be determined before Platform planning" }
$credentialOwnerMatches = (-not [string]::IsNullOrWhiteSpace($effectiveGitHubOwner) -and [string]::Equals([string]$facts.github_credential.owner, $effectiveGitHubOwner, [StringComparison]::OrdinalIgnoreCase))
if ($facts.github_credential.exists -and -not [string]::IsNullOrWhiteSpace([string]$facts.github_credential.owner) -and -not $credentialOwnerMatches) {
    throw "Existing Workflow Home is already bound to GitHub owner '$([string]$facts.github_credential.owner)' and cannot be rebound to '$effectiveGitHubOwner'"
}
$credentialCurrent = ($facts.github_credential.exists -and $facts.github_credential.verified -and $credentialOwnerMatches -and $observedScopes -contains "repo" -and $observedScopes -contains "workflow")
if (-not $credentialCurrent) {
    if ([string]::IsNullOrWhiteSpace($effectiveGitHubOwner)) { throw "GitHubOwner is required when the Control Plane PAT is not persisted" }
    $patAction = $(if ($facts.github_credential.exists) { "replace" } else { "persist" })
    $actions.Add([ordered]@{ id = "persist-classic-pat"; kind = "github_pat"; subject = $facts.github_credential.path; action = $patAction; parameters = (Add-PlatformPins ([ordered]@{ input = "stdin"; owner = $effectiveGitHubOwner })) })
}
$platformRecordCurrent = ($null -ne $facts.platform -and $facts.platform.installation_recorded -and [string]$facts.platform.version -eq [string]$manifest.release.version -and [string]$facts.platform.release_manifest_digest -eq $manifestDigest -and [string]$facts.platform.platform_setup_contract_digest -eq $platformSetupContractDigest -and [string]$facts.platform.workflow_cli_sha256 -eq [string]$workflowExecutable[0].sha256 -and [string]$facts.platform.release_bundled_files_digest -eq $releaseBundledFilesDigest -and [string]$facts.platform.release_bundled_files_json -eq $releaseBundledFilesJSON)
$controlPlaneAuthorizationCurrent = ($null -ne $facts.control_plane -and [string]$facts.control_plane.state -eq "ready" -and [string]$facts.control_plane.runtime.platform_version -eq [string]$manifest.release.version -and -not [string]::IsNullOrWhiteSpace([string]$facts.platform.control_plane_plan_digest_sha256) -and [string]$facts.platform.control_plane_plan_digest_sha256 -eq [string]$facts.control_plane.runtime.approved_platform_bootstrap_plan_digest_sha256)
$controlPlaneReady = ($platformRecordCurrent -and $controlPlaneAuthorizationCurrent)
if (-not $platformRecordCurrent) {
    $actions.Add([ordered]@{ id = "record-platform-installation"; kind = "platform_installation"; subject = $facts.workflow_home; action = "record"; parameters = [ordered]@{ version = [string]$manifest.release.version; release_manifest_digest = $manifestDigest; platform_setup_contract_json = $platformSetupContractJSON; platform_setup_contract_digest = $platformSetupContractDigest; workflow_cli_sha256 = [string]$workflowExecutable[0].sha256; release_bundled_files_json = $releaseBundledFilesJSON; release_bundled_files_digest = $releaseBundledFilesDigest } })
}
if (-not $controlPlaneReady) {
    $controlPlaneAction = $(if ([string]$facts.control_plane.state -eq "ready") { "replace" } else { "start" })
    $actions.Add([ordered]@{ id = "start-control-plane"; kind = "control_plane"; subject = $facts.workflow_home; action = $controlPlaneAction; parameters = [ordered]@{ version = [string]$manifest.release.version; release_manifest_digest = $manifestDigest; platform_setup_contract_digest = $platformSetupContractDigest; workflow_cli_sha256 = [string]$workflowExecutable[0].sha256; release_bundled_files_digest = $releaseBundledFilesDigest } })
}

$identitySeed = [ordered]@{
    kind = "platform_bootstrap"
    schema_version = 1
    target = [ordered]@{ workflow_home = $facts.workflow_home; repository_path = ""; github_repository = "" }
    preconditions = @(
        [ordered]@{ id = "platform-release"; kind = "platform_release"; subject = [string]$manifest.release.tag; expected = $manifestDigest }
        [ordered]@{ id = "platform-setup-contract"; kind = "platform_setup_contract"; subject = [string]$manifest.release.tag; expected = $platformSetupContractDigest }
    )
    effects = @($actions)
    expected_results = @([ordered]@{ id = "platform-ready"; kind = "platform_readiness"; subject = $facts.workflow_home; expected = "ready" })
}
$identityInput = Join-Path $scratchRoot "identity.input.json"; $identityCanonical = Join-Path $scratchRoot "identity.canonical.json"
[IO.File]::WriteAllText($identityInput, ($identitySeed | ConvertTo-Json -Depth 20 -Compress), (New-Object Text.UTF8Encoding($false)))
$identityDigest = (& $canonicalizer -InputPath $identityInput -OutputPath $identityCanonical | Select-Object -Last 1).Trim()
$seed = [ordered]@{
    plan_id = "setup-$($identityDigest.Substring(0, 24))"
    kind = $identitySeed.kind
    schema_version = $identitySeed.schema_version
    target = $identitySeed.target
    preconditions = $identitySeed.preconditions
    effects = $identitySeed.effects
    expected_results = $identitySeed.expected_results
}
$planInput = Join-Path $scratchRoot "plan.input.json"; $planCanonical = Join-Path $scratchRoot "plan.canonical.json"
[IO.File]::WriteAllText($planInput, ($seed | ConvertTo-Json -Depth 20 -Compress), (New-Object Text.UTF8Encoding($false)))
$digest = (& $canonicalizer -InputPath $planInput -OutputPath $planCanonical | Select-Object -Last 1).Trim()
$canonical = [IO.File]::ReadAllText($planCanonical, (New-Object Text.UTF8Encoding($false, $true)))
function Format-PlatformProjection($Plan, [string]$Digest) {
    $lines = [Collections.Generic.List[string]]::new()
    $lines.Add("Platform Bootstrap Plan $($Plan.plan_id)")
    $lines.Add("Target: $($Plan.target.workflow_home)")
    $lines.Add("Digest (SHA-256): $Digest")
    $lines.Add("")
    $lines.Add("Preconditions:")
    foreach ($precondition in $Plan.preconditions) { $lines.Add("- $($precondition.id): $($precondition.subject) ($($precondition.kind)) = $($precondition.expected)") }
    $lines.Add("")
    $lines.Add("Authorized effects:")
    if (@($Plan.effects).Count -eq 0) { $lines.Add("- none; the installed platform already matches the verified release") }
    foreach ($effect in $Plan.effects) {
        $lines.Add("- $($effect.id): $($effect.action) $($effect.subject) ($($effect.kind))")
        foreach ($parameter in $effect.parameters.PSObject.Properties | Sort-Object Name) {
            $value = [string]$parameter.Value
            if ($parameter.Name -eq "input") { $value = "<standard input; not stored in plan>" }
            $lines.Add("  $($parameter.Name): $value")
        }
    }
    $lines.Add("")
    $lines.Add("Expected results:")
    foreach ($result in $Plan.expected_results) { $lines.Add("- $($result.id): $($result.subject) ($($result.kind)) = $($result.expected)") }
    return ($lines -join [Environment]::NewLine)
}
$projection = Format-PlatformProjection $seed $digest
$envelope = [ordered]@{ status = $(if ($actions.Count -eq 0) { "ready" } else { "plan_required" }); digest_sha256 = $digest; canonical_json = $canonical; plan = $seed; projection = $projection }
$directory = Split-Path -Parent ([System.IO.Path]::GetFullPath($OutputPath))
if ($directory) { New-Item -ItemType Directory -Path $directory -Force | Out-Null }
$temporary = Join-Path $directory ("." + [System.IO.Path]::GetFileName($OutputPath) + "." + [Guid]::NewGuid().ToString("N") + ".tmp")
$envelopeJSON = $envelope | ConvertTo-Json -Depth 12
[IO.File]::WriteAllText($temporary, $envelopeJSON, (New-Object Text.UTF8Encoding($false)))
Move-Item -LiteralPath $temporary -Destination $OutputPath -Force
Remove-Item -LiteralPath $scratchRoot -Recurse -Force
$envelope | ConvertTo-Json -Depth 12
