[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$ManifestPath,
    [Parameter(Mandatory = $true)][string]$SignaturePath,
    [Parameter(Mandatory = $true)][string]$HostFactsPath,
    [Parameter(Mandatory = $true)][string]$OutputPath,
    [string]$GitHubOwner = "",
    [string]$PolicyPath = (Join-Path $PSScriptRoot "..\trust\release-policy.json"),
    [string]$PublicKeyPath = ""
)

$ErrorActionPreference = "Stop"
function Get-SHA256Hex([byte[]]$Bytes) {
    $hasher = [Security.Cryptography.SHA256]::Create()
    try { return ([BitConverter]::ToString($hasher.ComputeHash($Bytes))).Replace("-", "").ToLowerInvariant() } finally { $hasher.Dispose() }
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
if ($facts.schema_version -ne 1) { throw "Unsupported host-facts schema" }
if (-not $facts.supported_host) { throw "Agent Workflow setup supports Windows only" }

$actions = [System.Collections.Generic.List[object]]::new()
$manifestDigest = (Get-FileHash -LiteralPath $ManifestPath -Algorithm SHA256).Hash.ToLowerInvariant()
if (-not $facts.workflow.installed -or -not $facts.workflow.path_reconciled) {
    $actions.Add([ordered]@{ id = "install-platform-cli"; kind = "platform_cli"; subject = (Join-Path $facts.workflow_home "bin\workflow.exe"); action = "install"; parameters = [ordered]@{ version = $manifest.release.version } })
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
        if ((Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash.ToLowerInvariant() -ne [string]$file.sha256) { $bundleCurrent = $false; break }
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
        parameters = [ordered]@{
            version = [string]$manifest.platform_setup_contract.workflow_skill_bundle.version
            managed_skills_json = $managedSkillsJSON
            files_json = $skillFilesJSON
        }
    })
}
if (-not $facts.docker.installed) {
    $actions.Add([ordered]@{ id = "install-docker-desktop"; kind = "docker_desktop"; subject = "current-host"; action = "install"; parameters = [ordered]@{ version = $manifest.platform_setup_contract.docker_desktop.version; installer_url = $manifest.platform_setup_contract.docker_desktop.installer_url; windows_amd64_sha256 = $manifest.platform_setup_contract.docker_desktop.windows_amd64_sha256 } })
}
if (-not $facts.github_credential.exists) {
    if ([string]::IsNullOrWhiteSpace($GitHubOwner)) { throw "GitHubOwner is required when the Control Plane PAT is not persisted" }
    $actions.Add([ordered]@{ id = "persist-classic-pat"; kind = "github_pat"; subject = $facts.github_credential.path; action = "persist"; parameters = [ordered]@{ input = "stdin"; owner = $GitHubOwner } })
}
$platformSetupContractJSON = ConvertTo-Json -InputObject $manifest.platform_setup_contract -Depth 10 -Compress
$actions.Add([ordered]@{ id = "record-platform-installation"; kind = "platform_installation"; subject = $facts.workflow_home; action = "record"; parameters = [ordered]@{ version = $manifest.release.version; release_manifest_digest = $manifestDigest; platform_setup_contract_json = $platformSetupContractJSON } })
$controlPlaneReady = ($null -ne $facts.control_plane -and [string]$facts.control_plane.state -eq "ready" -and [string]$facts.control_plane.runtime.platform_version -eq [string]$manifest.release.version)
if (-not $controlPlaneReady) {
    $actions.Add([ordered]@{ id = "start-control-plane"; kind = "control_plane"; subject = $facts.workflow_home; action = "start"; parameters = [ordered]@{ version = $manifest.release.version } })
}

$identitySeed = [ordered]@{
    kind = "platform_bootstrap"
    schema_version = 1
    target = [ordered]@{ workflow_home = $facts.workflow_home; repository_path = ""; github_repository = "" }
    preconditions = @([ordered]@{ id = "platform-release"; kind = "platform_release_manifest"; subject = $manifest.release.tag; expected = $manifest.release.source_commit })
    effects = @($actions)
    expected_results = @([ordered]@{ id = "platform-ready"; kind = "platform_readiness"; subject = $facts.workflow_home; expected = "ready" })
}
$identityJSON = $identitySeed | ConvertTo-Json -Depth 10 -Compress
$identityBytes = [Text.Encoding]::UTF8.GetBytes($identityJSON)
$identityDigest = Get-SHA256Hex $identityBytes
$seed = [ordered]@{
    plan_id = "setup-$($identityDigest.Substring(0, 24))"
    kind = $identitySeed.kind
    schema_version = $identitySeed.schema_version
    target = $identitySeed.target
    preconditions = $identitySeed.preconditions
    effects = $identitySeed.effects
    expected_results = $identitySeed.expected_results
}
$canonical = $seed | ConvertTo-Json -Depth 10 -Compress
$bytes = [Text.Encoding]::UTF8.GetBytes($canonical)
$digest = Get-SHA256Hex $bytes
$envelope = [ordered]@{ status = $(if ($actions.Count -eq 0) { "ready" } else { "plan_required" }); digest_sha256 = $digest; plan = $seed; projection = ($seed | ConvertTo-Json -Depth 10) }
$directory = Split-Path -Parent ([System.IO.Path]::GetFullPath($OutputPath))
if ($directory) { New-Item -ItemType Directory -Path $directory -Force | Out-Null }
$temporary = Join-Path $directory ("." + [System.IO.Path]::GetFileName($OutputPath) + "." + [Guid]::NewGuid().ToString("N") + ".tmp")
$envelopeJSON = $envelope | ConvertTo-Json -Depth 12
[IO.File]::WriteAllText($temporary, $envelopeJSON, (New-Object Text.UTF8Encoding($false)))
Move-Item -LiteralPath $temporary -Destination $OutputPath -Force
$envelope | ConvertTo-Json -Depth 12
