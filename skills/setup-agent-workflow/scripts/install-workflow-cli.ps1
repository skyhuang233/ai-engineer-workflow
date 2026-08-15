[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$ManifestPath,
    [Parameter(Mandatory = $true)][string]$SignaturePath,
    [Parameter(Mandatory = $true)][string]$PlanPath,
    [Parameter(Mandatory = $true)][ValidatePattern('^[0-9a-f]{64}$')][string]$ApprovedDigest
)

$ErrorActionPreference = "Stop"
function Get-SHA256File([string]$Path) {
    $hasher = [Security.Cryptography.SHA256]::Create()
    try { return ([BitConverter]::ToString($hasher.ComputeHash([IO.File]::ReadAllBytes($Path)))).Replace("-", "").ToLowerInvariant() } finally { $hasher.Dispose() }
}
& (Join-Path $PSScriptRoot "verify-platform-release.ps1") -ManifestPath $ManifestPath -SignaturePath $SignaturePath | Out-Null

$manifest = Get-Content -LiteralPath $ManifestPath -Raw | ConvertFrom-Json
$planEnvelope = Get-Content -LiteralPath $PlanPath -Raw | ConvertFrom-Json
if ($planEnvelope.digest_sha256 -ne $ApprovedDigest) { throw "Approved digest does not match the Setup Plan" }
if ($manifest.schema_version -ne 1) { throw "Unsupported Platform Release Manifest schema" }
if ([int]$manifest.bootstrap_contract.minimum_schema -gt 1 -or [int]$manifest.bootstrap_contract.maximum_schema -lt 1) { throw "Platform Release is incompatible with this bootstrap planner" }
$canonicalPlan = [string]$planEnvelope.canonical_json
if ([string]::IsNullOrWhiteSpace($canonicalPlan)) { throw "Setup Plan envelope lacks canonical JSON" }
$approvedPlan = $canonicalPlan | ConvertFrom-Json
$manifestDigest = Get-SHA256File $ManifestPath

$asset = $manifest.artifacts | Where-Object { $_.name -eq "workflow-windows-amd64.zip" } | Select-Object -First 1
if ($null -eq $asset) { throw "Release has no Windows amd64 Workflow CLI asset" }
$temporaryRoot = Join-Path ([System.IO.Path]::GetTempPath()) ("AgentWorkflow\downloads\" + [Guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $temporaryRoot -Force | Out-Null
try {
    $canonicalInput = Join-Path $temporaryRoot "approved-plan.input.json"
    $canonicalPlanPath = Join-Path $temporaryRoot "approved-plan.json"
    [IO.File]::WriteAllText($canonicalInput, $canonicalPlan, (New-Object Text.UTF8Encoding($false)))
    $recomputedDigest = (& (Join-Path $PSScriptRoot "convert-to-setup-canonical-json.ps1") -InputPath $canonicalInput -OutputPath $canonicalPlanPath | Select-Object -Last 1).Trim()
    if ($recomputedDigest -ne $ApprovedDigest -or [IO.File]::ReadAllText($canonicalPlanPath) -ne $canonicalPlan) { throw "Approved canonical Setup Plan digest is invalid" }
    $releasePreconditions = @($approvedPlan.preconditions | Where-Object { $_.kind -eq "platform_release" -and [string]$_.expected -eq $manifestDigest })
    $contractInput = Join-Path $temporaryRoot "platform-contract.input.json"; $contractCanonical = Join-Path $temporaryRoot "platform-contract.canonical.json"
    [IO.File]::WriteAllText($contractInput, ($manifest.platform_setup_contract | ConvertTo-Json -Depth 20 -Compress), (New-Object Text.UTF8Encoding($false)))
    $contractDigest = (& (Join-Path $PSScriptRoot "convert-to-setup-canonical-json.ps1") -InputPath $contractInput -OutputPath $contractCanonical | Select-Object -Last 1).Trim()
    $bundledFilesInput = Join-Path $temporaryRoot "bundled-files.input.json"; $bundledFilesCanonical = Join-Path $temporaryRoot "bundled-files.canonical.json"
    [IO.File]::WriteAllText($bundledFilesInput, (ConvertTo-Json -InputObject @($manifest.bundled_files) -Depth 10 -Compress), (New-Object Text.UTF8Encoding($false)))
    $releaseBundledFilesDigest = (& (Join-Path $PSScriptRoot "convert-to-setup-canonical-json.ps1") -InputPath $bundledFilesInput -OutputPath $bundledFilesCanonical | Select-Object -Last 1).Trim()
    $releaseBundledFilesJSON = [IO.File]::ReadAllText($bundledFilesCanonical, (New-Object Text.UTF8Encoding($false, $true)))
    $contractPreconditions = @($approvedPlan.preconditions | Where-Object { $_.kind -eq "platform_setup_contract" -and [string]$_.expected -eq $contractDigest })
    $workflowExecutablePins = @($manifest.bundled_files | Where-Object { [string]$_.path -eq "bin/workflow.exe" })
    if ($workflowExecutablePins.Count -ne 1) { throw "Verified Platform Release has no exact Workflow CLI checksum" }
    $platformKinds = @("platform_cli", "workflow_skill_bundle", "docker_desktop", "github_pat", "platform_installation", "control_plane")
    $platformEffects = @($approvedPlan.effects | Where-Object { $platformKinds -contains [string]$_.kind })
    $boundEffects = @($platformEffects | Where-Object { [string]$_.parameters.release_manifest_digest -eq $manifestDigest -and [string]$_.parameters.platform_setup_contract_digest -eq $contractDigest -and [string]$_.parameters.workflow_cli_sha256 -eq [string]$workflowExecutablePins[0].sha256 -and [string]$_.parameters.release_bundled_files_digest -eq $releaseBundledFilesDigest })
    if ([string]$approvedPlan.kind -ne "platform_bootstrap" -or $releasePreconditions.Count -ne 1 -or $contractPreconditions.Count -ne 1 -or $platformEffects.Count -lt 1 -or $boundEffects.Count -ne $platformEffects.Count -or $platformEffects.Count -ne @($approvedPlan.effects).Count) { throw "Approved Setup Plan does not bind the verified Platform Release and contract" }
    foreach ($effect in $platformEffects) {
        switch ([string]$effect.kind) {
            "platform_cli" {
                if ([string]$effect.action -ne "install" -or [string]$effect.parameters.version -ne [string]$manifest.release.version -or [string]$effect.parameters.sha256 -ne [string]$workflowExecutablePins[0].sha256) { throw "Approved Setup Plan Platform CLI effect differs from the verified manifest" }
            }
            "workflow_skill_bundle" {
                if ([string]$effect.action -ne "install" -or [string]$effect.parameters.version -ne [string]$manifest.platform_setup_contract.workflow_skill_bundle.version) { throw "Approved Setup Plan Workflow Skill Bundle effect differs from the verified manifest" }
                $plannedSkills = @([string]$effect.parameters.managed_skills_json | ConvertFrom-Json | ForEach-Object { [string]$_ })
                $manifestSkills = @($manifest.platform_setup_contract.workflow_skill_bundle.managed_skills | ForEach-Object { [string]$_ })
                if ($plannedSkills.Count -ne $manifestSkills.Count) { throw "Approved Setup Plan Workflow Skill Bundle payload differs from the verified manifest" }
                for ($index = 0; $index -lt $manifestSkills.Count; $index++) { if ($plannedSkills[$index] -ne $manifestSkills[$index]) { throw "Approved Setup Plan Workflow Skill Bundle payload differs from the verified manifest" } }
                $plannedFiles = @([string]$effect.parameters.files_json | ConvertFrom-Json)
                $manifestFiles = @($manifest.bundled_files | Where-Object { ([string]$_.path).StartsWith("skills/") })
                if ($plannedFiles.Count -ne $manifestFiles.Count) { throw "Approved Setup Plan Workflow Skill Bundle payload differs from the verified manifest" }
                for ($index = 0; $index -lt $manifestFiles.Count; $index++) {
                    if ([string]$plannedFiles[$index].path -ne ([string]$manifestFiles[$index].path).Substring(7) -or [string]$plannedFiles[$index].sha256 -ne [string]$manifestFiles[$index].sha256) { throw "Approved Setup Plan Workflow Skill Bundle payload differs from the verified manifest" }
                }
            }
            "docker_desktop" {
                if (@("install", "upgrade", "repair") -notcontains [string]$effect.action -or [string]$effect.parameters.version -ne [string]$manifest.platform_setup_contract.docker_desktop.version -or [string]$effect.parameters.installer_url -ne [string]$manifest.platform_setup_contract.docker_desktop.installer_url -or [string]$effect.parameters.windows_amd64_sha256 -ne [string]$manifest.platform_setup_contract.docker_desktop.windows_amd64_sha256) { throw "Approved Setup Plan Docker Desktop effect differs from the verified manifest" }
            }
            "github_pat" {
                $credentialContract = $manifest.platform_setup_contract.credential
                $expectedCredentialPath = [IO.Path]::GetFullPath((Join-Path ([string]$approvedPlan.target.workflow_home) ([string]$credentialContract.plaintext_relative_path)))
                $requiredScopes = @($credentialContract.required_scopes | ForEach-Object { [string]$_ }) -join ","
                $actualParameterNames = @($effect.parameters.PSObject.Properties.Name | Sort-Object)
                $expectedParameterNames = @("input", "owner", "platform_setup_contract_digest", "release_bundled_files_digest", "release_manifest_digest", "required_scopes", "workflow_cli_sha256" | Sort-Object)
                if (@("persist", "replace") -notcontains [string]$effect.action -or [string]$effect.subject -cne $expectedCredentialPath -or [string]$effect.parameters.input -cne "stdin" -or [string]::IsNullOrWhiteSpace([string]$effect.parameters.owner) -or [string]$effect.parameters.owner -cne ([string]$effect.parameters.owner).Trim() -or [string]$effect.parameters.required_scopes -cne $requiredScopes -or ($actualParameterNames -join "`n") -cne ($expectedParameterNames -join "`n")) { throw "Approved Setup Plan GitHub PAT binding differs from the verified manifest" }
            }
            "platform_installation" {
                if ([string]$effect.action -ne "record" -or [string]$effect.parameters.version -ne [string]$manifest.release.version -or [string]$effect.parameters.platform_setup_contract_json -ne [IO.File]::ReadAllText($contractCanonical) -or [string]$effect.parameters.release_bundled_files_json -ne $releaseBundledFilesJSON) { throw "Approved Setup Plan Platform Installation effect differs from the verified manifest" }
            }
            "control_plane" {
                if (@("start", "replace") -notcontains [string]$effect.action -or [string]$effect.parameters.version -ne [string]$manifest.release.version) { throw "Approved Setup Plan Control Plane effect differs from the verified manifest" }
            }
        }
    }
    $archive = Join-Path $temporaryRoot $asset.name
    $assetURL = "https://github.com/$($manifest.release.repository)/releases/download/$($manifest.release.tag)/$($asset.name)"
    Invoke-WebRequest -Uri $assetURL -OutFile $archive -UseBasicParsing
    $actual = Get-SHA256File $archive
    if ($actual -ne $asset.sha256) { throw "Workflow CLI asset checksum mismatch" }
    $expanded = Join-Path $temporaryRoot "expanded"
    Expand-Archive -LiteralPath $archive -DestinationPath $expanded
    $executable = Get-ChildItem -LiteralPath $expanded -Filter workflow.exe -Recurse | Select-Object -First 1
    if ($null -eq $executable) { throw "Workflow CLI archive has no workflow.exe" }
    $patEffects = @($approvedPlan.effects | Where-Object { [string]$_.kind -eq "github_pat" })
    if ($patEffects.Count -gt 1) { throw "Approved Setup Plan contains multiple GitHub PAT effects" }
    if ($patEffects.Count -eq 1) {
        $pat = [Console]::In.ReadLine()
        if ([string]::IsNullOrWhiteSpace($pat)) { throw "Approved GitHub PAT effect requires standard input" }
        try { $pat | & $executable.FullName setup apply --plan $canonicalPlanPath --approved-digest $ApprovedDigest } finally { $pat = $null }
    } else {
        & $executable.FullName setup apply --plan $canonicalPlanPath --approved-digest $ApprovedDigest
    }
    if ($LASTEXITCODE -ne 0) { throw "workflow setup apply failed with exit code $LASTEXITCODE" }
    $workflowHome = [IO.Path]::GetFullPath([string]$approvedPlan.target.workflow_home)
    if (-not [IO.Path]::IsPathRooted($workflowHome) -or $workflowHome.StartsWith("\\")) { throw "Approved Setup Plan Workflow Home is not an absolute local path" }
    $pinDirectory = Join-Path $workflowHome "config"
    New-Item -ItemType Directory -Path $pinDirectory -Force | Out-Null
    $pinPath = Join-Path $pinDirectory "bootstrap-platform-release-pin.json"
    $pinTemporaryPath = $pinPath + ".tmp-" + [Guid]::NewGuid().ToString("N")
    $pin = [ordered]@{
        schema_version = 1
        release_version = [string]$manifest.release.version
        release_manifest_digest_sha256 = $manifestDigest
        platform_setup_contract_digest_sha256 = $contractDigest
        workflow_cli_sha256 = [string]$workflowExecutablePins[0].sha256
        manifest_base64 = [Convert]::ToBase64String([IO.File]::ReadAllBytes([IO.Path]::GetFullPath($ManifestPath)))
        signature_base64 = [Convert]::ToBase64String([IO.File]::ReadAllBytes([IO.Path]::GetFullPath($SignaturePath)))
    }
    try {
        [IO.File]::WriteAllText($pinTemporaryPath, ($pin | ConvertTo-Json -Compress), (New-Object Text.UTF8Encoding($false)))
        if (Test-Path -LiteralPath $pinPath -PathType Leaf) {
            [IO.File]::Replace($pinTemporaryPath, $pinPath, $null)
        } else {
            [IO.File]::Move($pinTemporaryPath, $pinPath)
        }
    } finally {
        Remove-Item -LiteralPath $pinTemporaryPath -Force -ErrorAction SilentlyContinue
    }
} finally {
    Remove-Item -LiteralPath $temporaryRoot -Recurse -Force -ErrorAction SilentlyContinue
}
