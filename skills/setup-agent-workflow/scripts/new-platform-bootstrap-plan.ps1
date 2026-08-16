[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$ManifestPath,
    [Parameter(Mandatory = $true)][string]$HostFactsPath,
    [Parameter(Mandatory = $true)][string]$OutputPath,
    [Parameter(Mandatory = $true)][string]$GitHubOwner,
    [ValidateSet("", "personal", "organization")][string]$GitHubOwnerType = "",
    [ValidatePattern('^[0-9a-f]{64}$')][string]$GitHubPATFingerprintSHA256 = "",
    [switch]$AllowUpgrade
)

$ErrorActionPreference = "Stop"
function Get-SHA256Hex([byte[]]$Bytes) {
    $hasher = [Security.Cryptography.SHA256]::Create()
    try { return ([BitConverter]::ToString($hasher.ComputeHash($Bytes))).Replace("-", "").ToLowerInvariant() } finally { $hasher.Dispose() }
}
function Get-SHA256File([string]$Path) {
    return Get-SHA256Hex ([IO.File]::ReadAllBytes($Path))
}
function Get-ComparableWorkflowPath([string]$Path) {
    $full = [IO.Path]::GetFullPath($Path)
    # Existing Windows paths may be reported through an 8.3 alias by one
    # process and their long form by another. Resolve an existing target to
    # its filesystem identity; planned paths retain their normalized form.
    if (Test-Path -LiteralPath $full) { return (Get-Item -LiteralPath $full).FullName }
    return $full
}
& (Join-Path $PSScriptRoot "verify-platform-release.ps1") -ManifestPath $ManifestPath | Out-Null

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
    $cliParameters = [ordered]@{ version = [string]$manifest.release.version; sha256 = [string]$workflowExecutable[0].sha256 }
    $actions.Add([ordered]@{ id = "install-platform-cli"; kind = "platform_cli"; subject = (Join-Path $facts.workflow_home "bin\workflow.exe"); action = "install"; parameters = (Add-PlatformPins $cliParameters) })
}
$managedSkills = @($manifest.platform_setup_contract.workflow_skill_bundle.managed_skills | ForEach-Object { [string]$_ })
$skillFiles = @($manifest.bundled_files | Where-Object { ([string]$_.path).StartsWith("skills/") } | ForEach-Object {
    [ordered]@{ path = ([string]$_.path).Substring(7); sha256 = [string]$_.sha256 }
})
if ($managedSkills.Count -eq 0 -or $skillFiles.Count -eq 0) { throw "Verified Platform Release has no Workflow Skill Bundle files" }
$expectedSkillFileDigests = [ordered]@{}
foreach ($file in $skillFiles) { $expectedSkillFileDigests[[string]$file.path] = [string]$file.sha256 }
$bundleCurrent = $true
$bundleStatePath = Join-Path $facts.workflow_home "config\workflow-skills.owner.json"
if (-not (Test-Path -LiteralPath $bundleStatePath -PathType Leaf)) {
    $bundleCurrent = $false
} else {
    try {
        $bundleState = Get-Content -LiteralPath $bundleStatePath -Raw | ConvertFrom-Json
        if ($bundleState.owner -ne "agent-workflow-platform") { throw "Existing Workflow Skill Bundle state is not owned by Agent Workflow" }
        if ([string]$bundleState.version -ne [string]$manifest.platform_setup_contract.workflow_skill_bundle.version) { $bundleCurrent = $false }
        $recordedSkills = @($bundleState.skills | ForEach-Object { [string]$_ } | Sort-Object)
        $expectedSkills = @($managedSkills | Sort-Object)
        if (($recordedSkills -join "`n") -cne ($expectedSkills -join "`n")) { $bundleCurrent = $false }
        $recordedDigestProperties = @($bundleState.file_digests.PSObject.Properties)
        if ($recordedDigestProperties.Count -ne $expectedSkillFileDigests.Count) {
            $bundleCurrent = $false
        } else {
            foreach ($expectedDigest in $expectedSkillFileDigests.GetEnumerator()) {
                $property = $bundleState.file_digests.PSObject.Properties[[string]$expectedDigest.Key]
                if ($null -eq $property -or [string]$property.Value -cne [string]$expectedDigest.Value) { $bundleCurrent = $false; break }
            }
        }
    } catch {
        if ([string]$_.Exception.Message -eq "Existing Workflow Skill Bundle state is not owned by Agent Workflow") { throw }
        $bundleCurrent = $false
    }
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
$effectiveOwnerType = $GitHubOwnerType
if ([string]::IsNullOrWhiteSpace($effectiveOwnerType) -and -not [string]::IsNullOrWhiteSpace([string]$facts.github_credential.login)) {
    $effectiveOwnerType = $(if ([string]::Equals([string]$facts.github_credential.login, $effectiveGitHubOwner, [StringComparison]::OrdinalIgnoreCase)) { "personal" } else { "organization" })
}
if ([string]::IsNullOrWhiteSpace($effectiveOwnerType)) { throw "GitHubOwnerType must come from the read-only PAT owner verification before Platform planning" }
$requiredCredentialScopes = @(& (Join-Path $PSScriptRoot "resolve-github-required-scopes.ps1") -OwnerType $effectiveOwnerType)
$expectedCredentialPath = [IO.Path]::GetFullPath((Join-Path ([string]$facts.workflow_home) ([string]$manifest.platform_setup_contract.credential.plaintext_relative_path)))
if (-not [string]::Equals([IO.Path]::GetFullPath([string]$facts.github_credential.path), $expectedCredentialPath, [StringComparison]::OrdinalIgnoreCase)) { throw "Host facts GitHub credential path differs from the Platform Setup Contract" }
$credentialScopesMatch = @($requiredCredentialScopes | Where-Object { $observedScopes -notcontains $_ }).Count -eq 0
$credentialCurrent = ($facts.github_credential.exists -and $facts.github_credential.verified -and $credentialOwnerMatches -and $credentialScopesMatch)
if (-not $credentialCurrent) {
    if ([string]::IsNullOrWhiteSpace($effectiveGitHubOwner)) { throw "GitHubOwner is required when the Control Plane PAT is not persisted" }
    $patAction = $(if ($facts.github_credential.exists) { "replace" } else { "persist" })
    if ($GitHubPATFingerprintSHA256 -notmatch '^[0-9a-f]{64}$') { throw "A verified GitHub PAT fingerprint is required before planning credential persistence" }
    $actions.Add([ordered]@{ id = "persist-classic-pat"; kind = "github_pat"; subject = $expectedCredentialPath; action = $patAction; parameters = (Add-PlatformPins ([ordered]@{ input = "stdin"; owner = $effectiveGitHubOwner; required_scopes = ($requiredCredentialScopes -join ","); fingerprint_sha256 = $GitHubPATFingerprintSHA256 })) })
}
$platformRecordCurrent = ($null -ne $facts.platform -and $facts.platform.installation_recorded -and [string]$facts.platform.version -eq [string]$manifest.release.version -and [string]$facts.platform.release_manifest_digest -eq $manifestDigest -and [string]$facts.platform.platform_setup_contract_digest -eq $platformSetupContractDigest -and [string]$facts.platform.workflow_cli_sha256 -eq [string]$workflowExecutable[0].sha256 -and [string]$facts.platform.release_bundled_files_digest -eq $releaseBundledFilesDigest -and [string]$facts.platform.release_bundled_files_json -eq $releaseBundledFilesJSON)
$controlPlaneAuthorizationCurrent = ($null -ne $facts.control_plane -and [string]$facts.control_plane.state -eq "ready" -and [string]$facts.control_plane.runtime.platform_version -eq [string]$manifest.release.version -and -not [string]::IsNullOrWhiteSpace([string]$facts.platform.control_plane_plan_digest_sha256) -and [string]$facts.platform.control_plane_plan_digest_sha256 -eq [string]$facts.control_plane.runtime.approved_platform_bootstrap_plan_digest_sha256)
$controlPlaneReady = ($platformRecordCurrent -and $controlPlaneAuthorizationCurrent)
if (-not $platformRecordCurrent) {
    $actions.Add([ordered]@{ id = "record-platform-installation"; kind = "platform_installation"; subject = $facts.workflow_home; action = "record"; parameters = [ordered]@{ version = [string]$manifest.release.version; release_manifest_digest = $manifestDigest; platform_setup_contract_json = $platformSetupContractJSON; platform_setup_contract_digest = $platformSetupContractDigest; workflow_cli_sha256 = [string]$workflowExecutable[0].sha256; release_bundled_files_json = $releaseBundledFilesJSON; release_bundled_files_digest = $releaseBundledFilesDigest } })
}
$cliTrustRepairRequiresControlPlaneReadback = (-not $workflowCurrent -and $platformRecordCurrent)
if (-not $controlPlaneReady) {
    $controlPlaneAction = $(if ([string]$facts.control_plane.state -eq "ready" -or $cliTrustRepairRequiresControlPlaneReadback) { "replace" } else { "start" })
    $actions.Add([ordered]@{ id = "start-control-plane"; kind = "control_plane"; subject = $facts.workflow_home; action = $controlPlaneAction; parameters = [ordered]@{ version = [string]$manifest.release.version; release_manifest_digest = $manifestDigest; platform_setup_contract_digest = $platformSetupContractDigest; workflow_cli_sha256 = [string]$workflowExecutable[0].sha256; release_bundled_files_digest = $releaseBundledFilesDigest } })
}

$platformPreconditions = @(
    [ordered]@{ id = "platform-release"; kind = "platform_release"; subject = [string]$manifest.release.tag; expected = $manifestDigest }
    [ordered]@{ id = "platform-setup-contract"; kind = "platform_setup_contract"; subject = [string]$manifest.release.tag; expected = $platformSetupContractDigest }
)
$hostUserID = [string]$facts.host_identity.user_id
$hostUsername = [string]$facts.host_identity.username
$workflowHomeOwnerID = [string]$facts.host_identity.workflow_home_owner_id
if ([string]::IsNullOrWhiteSpace($hostUserID) -or [string]::IsNullOrWhiteSpace($hostUsername) -or [string]::IsNullOrWhiteSpace($workflowHomeOwnerID)) {
    throw "A complete current-user and Workflow Home owner identity snapshot is required"
}
if (-not [string]::Equals($hostUserID, $workflowHomeOwnerID, [StringComparison]::OrdinalIgnoreCase)) {
    throw "The existing Workflow Home must be owned by the current Windows user"
}
$approvedHostIdentity = [ordered]@{ user_id = $hostUserID; username = $hostUsername; workflow_home = (Get-ComparableWorkflowPath ([string]$facts.workflow_home)); workflow_home_owner_id = $workflowHomeOwnerID }
$platformPreconditions += [ordered]@{ id = "windows-user-and-home-owner"; kind = "host_identity"; subject = "current-user"; expected = ($approvedHostIdentity | ConvertTo-Json -Compress) }
$codexAuthVerified = ($facts.codex_auth.verified -and [IO.Path]::IsPathRooted([string]$facts.codex_auth.source) -and [string]$facts.codex_auth.fingerprint_sha256 -match '^[0-9a-f]{64}$')
if (-not $codexAuthVerified) { throw "A supported verified Codex ChatGPT authentication snapshot is required" }
$platformState = [ordered]@{
    codex_auth = [ordered]@{ source = [IO.Path]::GetFullPath([string]$facts.codex_auth.source); fingerprint_sha256 = [string]$facts.codex_auth.fingerprint_sha256 }
}
if ($credentialCurrent) {
    if ([string]$facts.github_credential.fingerprint_sha256 -notmatch '^[0-9a-f]{64}$') { throw "Verified GitHub PAT snapshot lacks a fingerprint" }
    $platformState.github_pat = [ordered]@{ fingerprint_sha256 = [string]$facts.github_credential.fingerprint_sha256; owner = [string]$facts.github_credential.owner; scopes = @($observedScopes) }
}
$stateInput = Join-Path $scratchRoot "platform-state.input.json"; $stateCanonical = Join-Path $scratchRoot "platform-state.canonical.json"
[IO.File]::WriteAllText($stateInput, ($platformState | ConvertTo-Json -Depth 8 -Compress), (New-Object Text.UTF8Encoding($false)))
$platformStateDigest = (& $canonicalizer -InputPath $stateInput -OutputPath $stateCanonical | Select-Object -Last 1).Trim()
$platformPreconditions += [ordered]@{ id = "satisfied-platform-state"; kind = "platform_state"; subject = $facts.workflow_home; expected = $platformStateDigest }
if ($facts.platform.installation_recorded -and -not $platformRecordCurrent) {
    $priorInstallation = [ordered]@{
        version = [string]$facts.platform.version
        release_manifest_digest = [string]$facts.platform.release_manifest_digest
        platform_setup_contract_digest = [string]$facts.platform.platform_setup_contract_digest
        workflow_cli_sha256 = [string]$facts.platform.workflow_cli_sha256
        release_bundled_files_digest = [string]$facts.platform.release_bundled_files_digest
        control_plane_plan_digest_sha256 = [string]$facts.platform.control_plane_plan_digest_sha256
    }
    foreach ($value in $priorInstallation.Values) { if ([string]::IsNullOrWhiteSpace([string]$value)) { throw "Platform Installation repair requires every durable prior pin" } }
    $priorInput = Join-Path $scratchRoot "prior-installation.input.json"; $priorCanonical = Join-Path $scratchRoot "prior-installation.canonical.json"
    [IO.File]::WriteAllText($priorInput, ($priorInstallation | ConvertTo-Json -Compress), (New-Object Text.UTF8Encoding($false)))
    $priorDigest = (& $canonicalizer -InputPath $priorInput -OutputPath $priorCanonical | Select-Object -Last 1).Trim()
    $platformPreconditions += [ordered]@{ id = "installed-platform-transition"; kind = "platform_installation"; subject = $facts.workflow_home; expected = $priorDigest }
}

$identitySeed = [ordered]@{
    kind = "platform_bootstrap"
    schema_version = 1
    target = [ordered]@{ workflow_home = $facts.workflow_home; repository_path = ""; github_repository = "" }
    preconditions = @($platformPreconditions)
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
