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
    if ($releasePreconditions.Count -ne 1 -or $contractPreconditions.Count -ne 1 -or $platformEffects.Count -lt 1 -or $boundEffects.Count -ne $platformEffects.Count) { throw "Approved Setup Plan does not bind the verified Platform Release and contract" }
    $plannedVersions = @($approvedPlan.effects | Where-Object { $_.kind -eq "platform_cli" -and $_.action -eq "install" } | ForEach-Object { [string]$_.parameters.version })
    if ($plannedVersions.Count -gt 1 -or ($plannedVersions.Count -eq 1 -and $plannedVersions[0] -ne [string]$manifest.release.version)) { throw "Approved Setup Plan does not bind the verified Platform Release version" }
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
