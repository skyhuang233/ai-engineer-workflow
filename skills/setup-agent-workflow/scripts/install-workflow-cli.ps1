[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$ManifestPath,
    [Parameter(Mandatory = $true)][string]$PlanPath,
    [Parameter(Mandatory = $true)][ValidatePattern('^[0-9a-f]{64}$')][string]$ApprovedDigest
)

$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"
function Get-SHA256File([string]$Path) {
    $hasher = [Security.Cryptography.SHA256]::Create()
    try { return ([BitConverter]::ToString($hasher.ComputeHash([IO.File]::ReadAllBytes($Path)))).Replace("-", "").ToLowerInvariant() } finally { $hasher.Dispose() }
}
function Get-ReleaseAsset($Release, [string]$Name) {
    $matches = @($Release.assets | Where-Object { [string]$_.name -ceq $Name })
    if ($matches.Count -ne 1 -or [long]$matches[0].id -le 0) { throw "Platform Release lacks exact asset '$Name'" }
    return $matches[0]
}
function Invoke-WorkflowSetupApply([string]$Executable, [string]$Plan, [string]$Digest, [string]$PAT) {
    $start = New-Object Diagnostics.ProcessStartInfo
    $start.FileName = $Executable
    $start.Arguments = 'setup apply --plan "' + $Plan.Replace('"', '\"') + '" --approved-digest ' + $Digest
    $start.UseShellExecute = $false
    $start.RedirectStandardInput = $true
    $start.RedirectStandardOutput = $true
    $start.RedirectStandardError = $true
    $process = New-Object Diagnostics.Process
    $process.StartInfo = $start
    [void]$process.Start()
    $writer = New-Object IO.StreamWriter($process.StandardInput.BaseStream, (New-Object Text.UTF8Encoding($false)))
    try { $writer.WriteLine($PAT); $writer.Flush() } finally { $writer.Dispose() }
    $stdout = $process.StandardOutput.ReadToEnd(); $stderr = $process.StandardError.ReadToEnd()
    $process.WaitForExit()
    if ($process.ExitCode -ne 0) { throw "workflow setup apply failed with exit code $($process.ExitCode): $stderr$stdout" }
}
& (Join-Path $PSScriptRoot "verify-platform-release.ps1") -ManifestPath $ManifestPath | Out-Null

$manifest = Get-Content -LiteralPath $ManifestPath -Raw | ConvertFrom-Json
$planEnvelope = Get-Content -LiteralPath $PlanPath -Raw | ConvertFrom-Json
if ($planEnvelope.digest_sha256 -ne $ApprovedDigest) { throw "Approved digest does not match the Setup Plan" }
if ($manifest.schema_version -ne 1) { throw "Unsupported Platform Release Manifest schema" }
if ([int]$manifest.bootstrap_contract.minimum_schema -gt 1 -or [int]$manifest.bootstrap_contract.maximum_schema -lt 1) { throw "Platform Release is incompatible with this bootstrap planner" }
$canonicalPlan = [string]$planEnvelope.canonical_json
if ([string]::IsNullOrWhiteSpace($canonicalPlan)) { throw "Setup Plan envelope lacks canonical JSON" }
$approvedPlan = $canonicalPlan | ConvertFrom-Json
$manifestDigest = Get-SHA256File $ManifestPath

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
                $actualParameterNames = @($effect.parameters.PSObject.Properties.Name | Sort-Object)
                $expectedParameterNames = @("platform_setup_contract_digest", "release_bundled_files_digest", "release_manifest_digest", "sha256", "version", "workflow_cli_sha256" | Sort-Object)
                if ([string]$effect.action -ne "install" -or [string]$effect.parameters.version -ne [string]$manifest.release.version -or [string]$effect.parameters.sha256 -ne [string]$workflowExecutablePins[0].sha256 -or ($actualParameterNames -join "`n") -cne ($expectedParameterNames -join "`n")) { throw "Approved Setup Plan Platform CLI effect differs from the verified manifest" }
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
                $expectedParameterNames = @("fingerprint_sha256", "input", "owner", "platform_setup_contract_digest", "release_bundled_files_digest", "release_manifest_digest", "required_scopes", "workflow_cli_sha256" | Sort-Object)
                if (@("persist", "replace") -notcontains [string]$effect.action -or [string]$effect.subject -cne $expectedCredentialPath -or [string]$effect.parameters.input -cne "stdin" -or [string]::IsNullOrWhiteSpace([string]$effect.parameters.owner) -or [string]$effect.parameters.owner -cne ([string]$effect.parameters.owner).Trim() -or [string]$effect.parameters.required_scopes -cne $requiredScopes -or [string]$effect.parameters.fingerprint_sha256 -notmatch '^[0-9a-f]{64}$' -or ($actualParameterNames -join "`n") -cne ($expectedParameterNames -join "`n")) { throw "Approved Setup Plan GitHub PAT binding differs from the verified manifest" }
            }
            "platform_installation" {
                if ([string]$effect.action -ne "record" -or [string]$effect.parameters.version -ne [string]$manifest.release.version -or [string]$effect.parameters.platform_setup_contract_json -ne [IO.File]::ReadAllText($contractCanonical) -or [string]$effect.parameters.release_bundled_files_json -ne $releaseBundledFilesJSON) { throw "Approved Setup Plan Platform Installation effect differs from the verified manifest" }
            }
            "control_plane" {
                if (@("start", "replace") -notcontains [string]$effect.action -or [string]$effect.parameters.version -ne [string]$manifest.release.version) { throw "Approved Setup Plan Control Plane effect differs from the verified manifest" }
            }
        }
    }
    # The same explicitly supplied, one-use PAT authenticates both private
    # release asset reads and the Control Plane apply when that plan needs it.
    $releasePAT = [Console]::In.ReadLine()
    if ([string]::IsNullOrWhiteSpace($releasePAT)) { throw "Platform Release download requires a GitHub PAT on standard input" }
    $repository = [string]$manifest.release.repository
    $releaseHeaders = @{ Authorization = "Bearer $releasePAT"; Accept = "application/vnd.github+json"; "X-GitHub-Api-Version" = "2022-11-28"; "User-Agent" = "agent-workflow-bootstrap" }
    $releaseResponse = Invoke-WebRequest -Uri "https://api.github.com/repos/$repository/releases/tags/$([string]$manifest.release.tag)" -Headers $releaseHeaders -UseBasicParsing
    $release = [string]$releaseResponse.Content | ConvertFrom-Json
    $assetHeaders = @{ Authorization = "Bearer $releasePAT"; Accept = "application/octet-stream"; "X-GitHub-Api-Version" = "2022-11-28"; "User-Agent" = "agent-workflow-bootstrap" }
    $checksumPath = Join-Path $temporaryRoot "SHA256SUMS"
    $checksumAsset = Get-ReleaseAsset $release "SHA256SUMS"
    Invoke-WebRequest -Uri "https://api.github.com/repos/$repository/releases/assets/$([long]$checksumAsset.id)" -Headers $assetHeaders -OutFile $checksumPath -UseBasicParsing
    $checksumMatch = Select-String -LiteralPath $checksumPath -Pattern '^([0-9a-f]{64})  workflow-windows-amd64\.zip$'
    if (@($checksumMatch).Count -ne 1) { throw "SHA256SUMS lacks an exact workflow-windows-amd64.zip checksum" }
    $expectedArchiveSHA256 = [string]$checksumMatch[0].Matches[0].Groups[1].Value
    $archive = Join-Path $temporaryRoot "workflow-windows-amd64.zip"
    $archiveAsset = Get-ReleaseAsset $release "workflow-windows-amd64.zip"
    Invoke-WebRequest -Uri "https://api.github.com/repos/$repository/releases/assets/$([long]$archiveAsset.id)" -Headers $assetHeaders -OutFile $archive -UseBasicParsing
    $actual = Get-SHA256File $archive
    if ($actual -ne $expectedArchiveSHA256) { throw "Workflow CLI archive checksum differs from SHA256SUMS" }
    $expanded = Join-Path $temporaryRoot "expanded"
    Expand-Archive -LiteralPath $archive -DestinationPath $expanded
    $expectedExecutablePath = Join-Path $expanded "bin\workflow.exe"
    $workflowExecutableEntries = @(Get-ChildItem -LiteralPath $expanded -File -Recurse | Where-Object { [string]::Equals($_.Name, "workflow.exe", [StringComparison]::OrdinalIgnoreCase) })
    if ($workflowExecutableEntries.Count -ne 1 -or -not (Test-Path -LiteralPath $expectedExecutablePath -PathType Leaf) -or -not [string]::Equals([IO.Path]::GetFullPath($workflowExecutableEntries[0].FullName), [IO.Path]::GetFullPath($expectedExecutablePath), [StringComparison]::OrdinalIgnoreCase)) { throw "Workflow CLI archive must contain only exact bin/workflow.exe" }
    $executable = Get-Item -LiteralPath $expectedExecutablePath
    if ((Get-SHA256File $executable.FullName) -cne [string]$workflowExecutablePins[0].sha256) { throw "Workflow CLI executable checksum differs from the manifest workflow_cli_sha256" }
    $publishedVersion = (& $executable.FullName version | Out-String).Trim()
    if ($LASTEXITCODE -ne 0 -or $publishedVersion -cne ("workflow " + [string]$manifest.release.version)) { throw "Workflow CLI published version differs from the Platform Release Manifest" }
    $patEffects = @($approvedPlan.effects | Where-Object { [string]$_.kind -eq "github_pat" })
    if ($patEffects.Count -gt 1) { throw "Approved Setup Plan contains multiple GitHub PAT effects" }
    if ($patEffects.Count -eq 1) {
        Invoke-WorkflowSetupApply $executable.FullName $canonicalPlanPath $ApprovedDigest $releasePAT
    } else {
        & $executable.FullName setup apply --plan $canonicalPlanPath --approved-digest $ApprovedDigest
    }
    if ($patEffects.Count -eq 0 -and $LASTEXITCODE -ne 0) { throw "workflow setup apply failed with exit code $LASTEXITCODE" }
    $workflowHome = [IO.Path]::GetFullPath([string]$approvedPlan.target.workflow_home)
    if (-not [IO.Path]::IsPathRooted($workflowHome) -or $workflowHome.StartsWith("\\")) { throw "Approved Setup Plan Workflow Home is not an absolute local path" }
    $inspectionOutput = (& $executable.FullName setup inspect-platform --workflow-home $workflowHome | Out-String).Trim()
    if ($LASTEXITCODE -ne 0) { throw "post-apply Platform Installation readback failed with exit code $LASTEXITCODE" }
    try { $inspection = $inspectionOutput | ConvertFrom-Json } catch { throw "post-apply Platform Installation readback is invalid JSON" }
    $installedPlatform = $inspection.result.platform
    if ([string]$inspection.status -ne "ready" -or $null -eq $installedPlatform -or -not [bool]$inspection.result.workflow_cli.verified) { throw "post-apply Platform Installation readback is not verified" }
    if (-not [bool]$installedPlatform.installation_recorded -or [string]$installedPlatform.version -cne [string]$manifest.release.version -or [string]$installedPlatform.release_manifest_digest -cne $manifestDigest -or [string]$installedPlatform.platform_setup_contract_digest -cne $contractDigest -or [string]$installedPlatform.workflow_cli_sha256 -cne [string]$workflowExecutablePins[0].sha256 -or [string]$installedPlatform.release_bundled_files_json -cne $releaseBundledFilesJSON -or [string]$installedPlatform.release_bundled_files_digest -cne $releaseBundledFilesDigest) { throw "post-apply Platform Installation differs from the verified release" }
    $controlPlaneAuthorizationDigest = [string]$installedPlatform.control_plane_plan_digest_sha256
    if ($controlPlaneAuthorizationDigest -notmatch '^[0-9a-f]{64}$') { throw "post-apply Platform Installation lacks a Control Plane authorization digest" }
    $statusOutput = (& $executable.FullName status --workflow-home $workflowHome | Out-String).Trim()
    if ($LASTEXITCODE -ne 0) { throw "post-apply Control Plane readback failed with exit code $LASTEXITCODE" }
    try { $liveControlPlane = $statusOutput | ConvertFrom-Json } catch { throw "post-apply Control Plane readback is invalid JSON" }
    if ([string]$liveControlPlane.state -ne "ready" -or [string]$liveControlPlane.runtime.platform_version -cne [string]$manifest.release.version -or [string]$liveControlPlane.runtime.approved_platform_bootstrap_plan_digest_sha256 -cne $controlPlaneAuthorizationDigest) { throw "post-apply live Control Plane differs from its durable Platform Installation authorization" }
    $controlPlaneEffects = @($approvedPlan.effects | Where-Object { [string]$_.kind -eq "control_plane" })
    if ($controlPlaneEffects.Count -gt 1 -or ($controlPlaneEffects.Count -eq 1 -and $controlPlaneAuthorizationDigest -cne $ApprovedDigest)) { throw "post-apply Control Plane authorization differs from the approved replacement" }
    $pinDirectory = Join-Path $workflowHome "config"
    $pinBackupDirectory = Join-Path $workflowHome "backups"
    New-Item -ItemType Directory -Path $pinDirectory -Force | Out-Null
    New-Item -ItemType Directory -Path $pinBackupDirectory -Force | Out-Null
    $pinPath = Join-Path $pinDirectory "bootstrap-platform-release-pin.json"
    $pinBackupPath = Join-Path $pinBackupDirectory "bootstrap-platform-release-pin.json"
    $pin = [ordered]@{
        schema_version = 1
        release_version = [string]$manifest.release.version
        release_manifest_digest_sha256 = $manifestDigest
        platform_setup_contract_digest_sha256 = $contractDigest
        workflow_cli_sha256 = [string]$workflowExecutablePins[0].sha256
        release_bundled_files_json = $releaseBundledFilesJSON
        release_bundled_files_digest_sha256 = $releaseBundledFilesDigest
        control_plane_plan_digest_sha256 = $controlPlaneAuthorizationDigest
        manifest_base64 = [Convert]::ToBase64String([IO.File]::ReadAllBytes([IO.Path]::GetFullPath($ManifestPath)))
    }
    $pinJSON = $pin | ConvertTo-Json -Compress
    foreach ($durablePinPath in @($pinBackupPath, $pinPath)) {
        $pinTemporaryPath = $durablePinPath + ".tmp-" + [Guid]::NewGuid().ToString("N")
        $replacedPinPath = $durablePinPath + ".replaced-" + [Guid]::NewGuid().ToString("N")
        try {
            [IO.File]::WriteAllText($pinTemporaryPath, $pinJSON, (New-Object Text.UTF8Encoding($false)))
            if (Test-Path -LiteralPath $durablePinPath -PathType Leaf) {
                [IO.File]::Replace($pinTemporaryPath, $durablePinPath, $replacedPinPath)
            } else {
                [IO.File]::Move($pinTemporaryPath, $durablePinPath)
            }
        } finally {
            Remove-Item -LiteralPath $pinTemporaryPath -Force -ErrorAction SilentlyContinue
            Remove-Item -LiteralPath $replacedPinPath -Force -ErrorAction SilentlyContinue
        }
    }
} finally {
    $releasePAT = $null
    Remove-Item -LiteralPath $temporaryRoot -Recurse -Force -ErrorAction SilentlyContinue
}
