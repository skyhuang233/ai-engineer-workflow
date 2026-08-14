[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$ManifestPath,
    [Parameter(Mandatory = $true)][string]$PlanPath,
    [Parameter(Mandatory = $true)][ValidatePattern('^[0-9a-f]{64}$')][string]$ApprovedDigest
)

$ErrorActionPreference = "Stop"
$manifest = Get-Content -LiteralPath $ManifestPath -Raw | ConvertFrom-Json
$planEnvelope = Get-Content -LiteralPath $PlanPath -Raw | ConvertFrom-Json
if ($planEnvelope.digest_sha256 -ne $ApprovedDigest) { throw "Approved digest does not match the Setup Plan" }
if ($manifest.schema_version -ne 1) { throw "Unsupported Platform Release Manifest schema" }

$asset = $manifest.artifacts | Where-Object { $_.name -eq "workflow-windows-amd64.zip" } | Select-Object -First 1
if ($null -eq $asset) { throw "Release has no Windows amd64 Workflow CLI asset" }
$temporaryRoot = Join-Path ([System.IO.Path]::GetTempPath()) ("AgentWorkflow\downloads\" + [Guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $temporaryRoot -Force | Out-Null
try {
    $archive = Join-Path $temporaryRoot $asset.name
    $assetURL = "https://github.com/$($manifest.release.repository)/releases/download/$($manifest.release.tag)/$($asset.name)"
    Invoke-WebRequest -Uri $assetURL -OutFile $archive -UseBasicParsing
    $actual = (Get-FileHash -LiteralPath $archive -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($actual -ne $asset.sha256) { throw "Workflow CLI asset checksum mismatch" }
    $expanded = Join-Path $temporaryRoot "expanded"
    Expand-Archive -LiteralPath $archive -DestinationPath $expanded
    $executable = Get-ChildItem -LiteralPath $expanded -Filter workflow.exe -Recurse | Select-Object -First 1
    if ($null -eq $executable) { throw "Workflow CLI archive has no workflow.exe" }
    $canonicalPlanPath = Join-Path $temporaryRoot "approved-plan.json"
    $planEnvelope.plan | ConvertTo-Json -Depth 20 -Compress | Set-Content -LiteralPath $canonicalPlanPath -Encoding utf8NoBOM
    & $executable.FullName setup apply --plan $canonicalPlanPath --approved-digest $ApprovedDigest
    if ($LASTEXITCODE -ne 0) { throw "workflow setup apply failed with exit code $LASTEXITCODE" }
} finally {
    Remove-Item -LiteralPath $temporaryRoot -Recurse -Force -ErrorAction SilentlyContinue
}
