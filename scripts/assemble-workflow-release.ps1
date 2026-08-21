[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [ValidatePattern('^[0-9a-f]{40}$')]
    [string]$CandidateSourceCommit,

    [Parameter(Mandatory = $true)]
    [ValidateRange(1, [long]::MaxValue)]
    [long]$QualificationRunID,

    [Parameter(Mandatory = $true)]
    [ValidateRange(1, [long]::MaxValue)]
    [long]$QualificationRunAttempt,

    [Parameter(Mandatory = $true)]
    [ValidatePattern('^ghcr\.io/skyhuang233/workflow-worker@sha256:[0-9a-f]{64}$')]
    [string]$WorkerImage,

    [Parameter(Mandatory = $true)]
    [string]$SBOMPath,

    [Parameter(Mandatory = $true)]
    [string]$OutputDirectory
)

$ErrorActionPreference = 'Stop'
$repositoryRoot = Split-Path -Parent $PSScriptRoot
$resolvedSBOM = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($SBOMPath)
$resolvedOutput = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($OutputDirectory)
$buildRoot = Join-Path $repositoryRoot 'build/workflow-release-staging'
$payloadRoot = Join-Path $buildRoot 'payload'
$workflowExecutable = Join-Path $buildRoot 'workflow.exe'
$setupExecutable = Join-Path $buildRoot 'workflow-setup.exe'
$workflowVersionExecutable = Join-Path $buildRoot $(if ($IsWindows) { 'workflow-version-check.exe' } else { 'workflow-version-check' })

if (-not (Test-Path -LiteralPath $resolvedSBOM -PathType Leaf)) {
    throw "Worker SBOM does not exist: $resolvedSBOM"
}
if (Test-Path -LiteralPath $resolvedOutput) {
    $outputEntries = @(Get-ChildItem -LiteralPath $resolvedOutput -Force)
    if ($outputEntries.Count -ne 0) {
        throw 'Workflow Release output directory must be empty'
    }
} else {
    New-Item -ItemType Directory -Path $resolvedOutput | Out-Null
}
if (Test-Path -LiteralPath $buildRoot) {
    Remove-Item -LiteralPath $buildRoot -Recurse -Force
}
New-Item -ItemType Directory -Path (Join-Path $payloadRoot 'repository-contract'),(Join-Path $payloadRoot 'skills') | Out-Null

$version = [string](Get-Content (Join-Path $repositoryRoot 'config/workflow-release.json') -Raw | ConvertFrom-Json).version
$env:CGO_ENABLED = '0'
Push-Location $repositoryRoot
try {
    $hostOS = (go env GOHOSTOS).Trim()
    if ($LASTEXITCODE -ne 0) { throw 'resolve Go host OS failed' }
    $hostArchitecture = (go env GOHOSTARCH).Trim()
    if ($LASTEXITCODE -ne 0) { throw 'resolve Go host architecture failed' }
    $env:GOOS = $hostOS
    $env:GOARCH = $hostArchitecture
    go build -trimpath -ldflags "-buildid= -X main.Version=$version" -o $workflowVersionExecutable ./cmd/workflow
    if ($LASTEXITCODE -ne 0) { throw 'build host Workflow version check failed' }
    $env:GOOS = 'windows'
    $env:GOARCH = 'amd64'
    go build -trimpath -ldflags "-buildid= -X main.Version=$version" -o $workflowExecutable ./cmd/workflow
    if ($LASTEXITCODE -ne 0) { throw 'build workflow.exe failed' }
    go build -trimpath -ldflags '-buildid=' -o $setupExecutable ./cmd/workflow-setup
    if ($LASTEXITCODE -ne 0) { throw 'build workflow-setup.exe failed' }
    Copy-Item -Recurse -Force deploy/platform/repository-contract/* (Join-Path $payloadRoot 'repository-contract')
    Copy-Item -Recurse -Force deploy/platform/skills/* (Join-Path $payloadRoot 'skills')
    $env:GOOS = $hostOS
    $env:GOARCH = $hostArchitecture
    go run ./cmd/workflow-release assemble `
        -config config/workflow-release.json -toolchain config/toolchain.json `
        -workflow-exe $workflowExecutable -workflow-version-exe $workflowVersionExecutable `
        -setup-exe $setupExecutable -payload $payloadRoot `
        -output $resolvedOutput -candidate-source-commit $CandidateSourceCommit `
        -qualification-run-id $QualificationRunID -qualification-run-attempt $QualificationRunAttempt -worker-image $WorkerImage -sbom $resolvedSBOM
    if ($LASTEXITCODE -ne 0) { throw 'assemble Workflow Release failed' }
} finally {
    Pop-Location
}

$bundlePath = Join-Path $resolvedOutput 'workflow-windows-amd64.zip'
$manifestPath = Join-Path $resolvedOutput 'workflow-release.json'
$stagedSBOMPath = Join-Path $resolvedOutput 'worker-sbom.spdx.json'
$bundleDigest = (Get-FileHash -LiteralPath $bundlePath -Algorithm SHA256).Hash.ToLowerInvariant()
$sbomDigest = (Get-FileHash -LiteralPath $stagedSBOMPath -Algorithm SHA256).Hash.ToLowerInvariant()
$manifest = Get-Content -LiteralPath $manifestPath -Raw | ConvertFrom-Json
if ([string]$manifest.candidate_source_commit -cne $CandidateSourceCommit -or [long]$manifest.qualification_run_id -ne $QualificationRunID -or [long]$manifest.qualification_run_attempt -ne $QualificationRunAttempt) {
    throw 'Workflow Release manifest source identity differs from assembly inputs'
}
if ([string]$manifest.worker.image -cne $WorkerImage) {
    throw 'Workflow Release manifest Worker image differs from assembly input'
}
if ([string]$manifest.bundle.name -cne 'workflow-windows-amd64.zip' -or [string]$manifest.bundle.sha256 -cne $bundleDigest) {
    throw 'Workflow Release manifest Bundle digest differs from assembled asset'
}
if ([string]$manifest.sbom.name -cne 'worker-sbom.spdx.json' -or [string]$manifest.sbom.sha256 -cne $sbomDigest) {
    throw 'Workflow Release manifest SBOM digest differs from assembled asset'
}
& (Join-Path $repositoryRoot 'skills/setup-agent-workflow/scripts/verify-windows-bundle.ps1') `
    -BundlePath $bundlePath -ExpectedSHA256 $bundleDigest -ExpectedVersion $version -ExpectedWorkerImage $WorkerImage | Out-Null
$assetNames = @(Get-ChildItem -LiteralPath $resolvedOutput -File | Sort-Object Name | ForEach-Object Name)
if (($assetNames -join ' ') -cne 'worker-sbom.spdx.json workflow-release.json workflow-windows-amd64.zip') {
    throw 'Workflow Release output is not the exact three-asset set'
}
