[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$ManifestPath,
    [Parameter(Mandatory = $true)][string]$SignaturePath,
    [Parameter(Mandatory = $true)][string]$PlanPath,
    [Parameter(Mandatory = $true)][ValidatePattern('^[0-9a-f]{64}$')][string]$ApprovedDigest,
    [string]$PolicyPath = (Join-Path $PSScriptRoot "..\trust\release-policy.json"),
    [string]$PublicKeyPath = ""
)

$ErrorActionPreference = "Stop"
$verificationArguments = @{
    ManifestPath = $ManifestPath
    SignaturePath = $SignaturePath
    PolicyPath = $PolicyPath
}
if (-not [string]::IsNullOrWhiteSpace($PublicKeyPath)) { $verificationArguments.PublicKeyPath = $PublicKeyPath }
& (Join-Path $PSScriptRoot "verify-platform-release.ps1") @verificationArguments | Out-Null

$manifest = Get-Content -LiteralPath $ManifestPath -Raw | ConvertFrom-Json
$planEnvelope = Get-Content -LiteralPath $PlanPath -Raw | ConvertFrom-Json
if ($planEnvelope.digest_sha256 -ne $ApprovedDigest) { throw "Approved digest does not match the Setup Plan" }
if ($manifest.schema_version -ne 1) { throw "Unsupported Platform Release Manifest schema" }
if ([int]$manifest.bootstrap_contract.minimum_schema -gt 1 -or [int]$manifest.bootstrap_contract.maximum_schema -lt 1) { throw "Platform Release is incompatible with this bootstrap planner" }
$canonicalPlan = [string]$planEnvelope.canonical_json
if ([string]::IsNullOrWhiteSpace($canonicalPlan)) { throw "Setup Plan envelope lacks canonical JSON" }
$approvedPlan = $canonicalPlan | ConvertFrom-Json
$manifestDigest = (Get-FileHash -LiteralPath $ManifestPath -Algorithm SHA256).Hash.ToLowerInvariant()

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
    $contractPreconditions = @($approvedPlan.preconditions | Where-Object { $_.kind -eq "platform_setup_contract" -and [string]$_.expected -eq $contractDigest })
    $workflowExecutablePins = @($manifest.bundled_files | Where-Object { [string]$_.path -eq "bin/workflow.exe" })
    if ($workflowExecutablePins.Count -ne 1) { throw "Verified Platform Release has no exact Workflow CLI checksum" }
    $platformKinds = @("platform_cli", "workflow_skill_bundle", "docker_desktop", "github_pat", "platform_installation", "control_plane")
    $platformEffects = @($approvedPlan.effects | Where-Object { $platformKinds -contains [string]$_.kind })
    $boundEffects = @($platformEffects | Where-Object { [string]$_.parameters.release_manifest_digest -eq $manifestDigest -and [string]$_.parameters.platform_setup_contract_digest -eq $contractDigest -and [string]$_.parameters.workflow_cli_sha256 -eq [string]$workflowExecutablePins[0].sha256 })
    if ([string]$approvedPlan.kind -ne "platform_bootstrap" -or $releasePreconditions.Count -ne 1 -or $contractPreconditions.Count -ne 1 -or $platformEffects.Count -lt 1 -or $boundEffects.Count -ne $platformEffects.Count -or $platformEffects.Count -ne @($approvedPlan.effects).Count) { throw "Approved Setup Plan does not bind the verified Platform Release and contract" }
    foreach ($effect in $platformEffects) {
        switch ([string]$effect.kind) {
            "platform_cli" {
                if ([string]$effect.action -ne "install" -or [string]$effect.parameters.version -ne [string]$manifest.release.version -or [string]$effect.parameters.sha256 -ne [string]$workflowExecutablePins[0].sha256) { throw "Approved Setup Plan Platform CLI effect differs from the verified manifest" }
            }
            "workflow_skill_bundle" {
                if ([string]$effect.action -ne "install" -or [string]$effect.parameters.version -ne [string]$manifest.platform_setup_contract.workflow_skill_bundle.version) { throw "Approved Setup Plan Workflow Skill Bundle effect differs from the verified manifest" }
            }
            "docker_desktop" {
                if (@("install", "upgrade", "repair") -notcontains [string]$effect.action -or [string]$effect.parameters.version -ne [string]$manifest.platform_setup_contract.docker_desktop.version -or [string]$effect.parameters.installer_url -ne [string]$manifest.platform_setup_contract.docker_desktop.installer_url -or [string]$effect.parameters.windows_amd64_sha256 -ne [string]$manifest.platform_setup_contract.docker_desktop.windows_amd64_sha256) { throw "Approved Setup Plan Docker Desktop effect differs from the verified manifest" }
            }
            "github_pat" {
                if (@("persist", "replace") -notcontains [string]$effect.action) { throw "Approved Setup Plan GitHub PAT action is invalid" }
            }
            "platform_installation" {
                if ([string]$effect.action -ne "record" -or [string]$effect.parameters.version -ne [string]$manifest.release.version -or [string]$effect.parameters.platform_setup_contract_json -ne [IO.File]::ReadAllText($contractCanonical)) { throw "Approved Setup Plan Platform Installation effect differs from the verified manifest" }
            }
            "control_plane" {
                if (@("start", "replace") -notcontains [string]$effect.action -or [string]$effect.parameters.version -ne [string]$manifest.release.version) { throw "Approved Setup Plan Control Plane effect differs from the verified manifest" }
            }
        }
    }
    $archive = Join-Path $temporaryRoot $asset.name
    $assetURL = "https://github.com/$($manifest.release.repository)/releases/download/$($manifest.release.tag)/$($asset.name)"
    Invoke-WebRequest -Uri $assetURL -OutFile $archive -UseBasicParsing
    $actual = (Get-FileHash -LiteralPath $archive -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($actual -ne $asset.sha256) { throw "Workflow CLI asset checksum mismatch" }
    $expanded = Join-Path $temporaryRoot "expanded"
    Expand-Archive -LiteralPath $archive -DestinationPath $expanded
    $executable = Get-ChildItem -LiteralPath $expanded -Filter workflow.exe -Recurse | Select-Object -First 1
    if ($null -eq $executable) { throw "Workflow CLI archive has no workflow.exe" }
    & $executable.FullName setup apply --plan $canonicalPlanPath --approved-digest $ApprovedDigest
    if ($LASTEXITCODE -ne 0) { throw "workflow setup apply failed with exit code $LASTEXITCODE" }
} finally {
    Remove-Item -LiteralPath $temporaryRoot -Recurse -Force -ErrorAction SilentlyContinue
}
