[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$ManifestPath,
    [Parameter(Mandatory = $true)][string]$SignaturePath,
    [string]$PolicyPath = (Join-Path $PSScriptRoot "..\trust\release-policy.json"),
    [string]$PublicKeyPath = ""
)

$ErrorActionPreference = "Stop"

function Assert-PlatformRelease([bool]$Condition, [string]$Message) {
    if (-not $Condition) { throw $Message }
}

function Get-RequiredProperty($Object, [string]$Name) {
    $property = $Object.PSObject.Properties[$Name]
    if ($null -eq $property) { throw "Platform Release metadata lacks required field '$Name'" }
    return $property.Value
}

function Get-SemanticVersion([string]$Value) {
    Assert-PlatformRelease ($Value -match '^v?(\d+)\.(\d+)\.(\d+)$') "Stable Platform Release version is invalid"
    return [Version]::new([int]$Matches[1], [int]$Matches[2], [int]$Matches[3])
}

function Convert-DerEcdsaSignature([byte[]]$Signature) {
    $offset = 0
    if ($Signature.Length -lt 8 -or $Signature[$offset++] -ne 0x30) { throw "Platform Release signature is not DER ECDSA" }
    $sequenceLength = [int]$Signature[$offset++]
    if (($sequenceLength -band 0x80) -ne 0) {
        $lengthBytes = $sequenceLength -band 0x7f
        if ($lengthBytes -lt 1 -or $lengthBytes -gt 2) { throw "Platform Release signature DER length is invalid" }
        $sequenceLength = 0
        for ($i = 0; $i -lt $lengthBytes; $i++) { $sequenceLength = ($sequenceLength -shl 8) -bor $Signature[$offset++] }
    }
    if ($offset + $sequenceLength -ne $Signature.Length) { throw "Platform Release signature DER sequence is truncated" }
    $scalars = [System.Collections.Generic.List[byte[]]]::new()
    for ($scalarIndex = 0; $scalarIndex -lt 2; $scalarIndex++) {
        if ($Signature[$offset++] -ne 0x02) { throw "Platform Release signature DER scalar is invalid" }
        $length = [int]$Signature[$offset++]
        if ($length -lt 1 -or $offset + $length -gt $Signature.Length) { throw "Platform Release signature DER scalar is truncated" }
        [byte[]]$value = $Signature[$offset..($offset + $length - 1)]
        $offset += $length
        while ($value.Length -gt 32 -and $value[0] -eq 0) { [byte[]]$value = $value[1..($value.Length - 1)] }
        if ($value.Length -gt 32) { throw "Platform Release signature DER scalar exceeds P-256" }
        [byte[]]$padded = New-Object byte[] 32
        [Array]::Copy($value, 0, $padded, 32 - $value.Length, $value.Length)
        $scalars.Add($padded)
    }
    if ($offset -ne $Signature.Length) { throw "Platform Release signature DER has trailing data" }
    [byte[]]$raw = New-Object byte[] 64
    [Array]::Copy($scalars[0], 0, $raw, 0, 32)
    [Array]::Copy($scalars[1], 0, $raw, 32, 32)
    return $raw
}

function Import-PinnedP256PublicKey([string]$PEM) {
    $base64 = (($PEM -split "`r?`n") | Where-Object { $_ -and -not $_.StartsWith("-----") }) -join ""
    [byte[]]$der = [Convert]::FromBase64String($base64)
    [byte[]]$prefix = @(0x30,0x59,0x30,0x13,0x06,0x07,0x2a,0x86,0x48,0xce,0x3d,0x02,0x01,0x06,0x08,0x2a,0x86,0x48,0xce,0x3d,0x03,0x01,0x07,0x03,0x42,0x00,0x04)
    if ($der.Length -ne ($prefix.Length + 64)) { throw "Pinned Platform Release public key is not P-256 SubjectPublicKeyInfo" }
    for ($i = 0; $i -lt $prefix.Length; $i++) {
        if ($der[$i] -ne $prefix[$i]) { throw "Pinned Platform Release public key is not ECDSA P-256" }
    }
    [byte[]]$blob = New-Object byte[] 72
    [Array]::Copy([BitConverter]::GetBytes([uint32]0x31534345), 0, $blob, 0, 4)
    [Array]::Copy([BitConverter]::GetBytes([uint32]32), 0, $blob, 4, 4)
    [Array]::Copy($der, $prefix.Length, $blob, 8, 64)
    $key = [Security.Cryptography.CngKey]::Import($blob, [Security.Cryptography.CngKeyBlobFormat]::EccPublicBlob)
    $ecdsa = New-Object Security.Cryptography.ECDsaCng $key
    $ecdsa.HashAlgorithm = [Security.Cryptography.CngAlgorithm]::Sha256
    return $ecdsa
}

foreach ($path in @($ManifestPath, $SignaturePath, $PolicyPath)) {
    Assert-PlatformRelease (Test-Path -LiteralPath $path -PathType Leaf) "Required Platform Release trust input is missing: $path"
}
$policy = Get-Content -LiteralPath $PolicyPath -Raw | ConvertFrom-Json
Assert-PlatformRelease ($policy.schema_version -eq 1) "Unsupported Platform Release trust policy"
if ([string]::IsNullOrWhiteSpace($PublicKeyPath)) {
    $PublicKeyPath = Join-Path (Split-Path -Parent ([IO.Path]::GetFullPath($PolicyPath))) ([string]$policy.public_key_file)
}
Assert-PlatformRelease (Test-Path -LiteralPath $PublicKeyPath -PathType Leaf) "Pinned Platform Release public key is missing: $PublicKeyPath"

$manifestBytes = [IO.File]::ReadAllBytes([IO.Path]::GetFullPath($ManifestPath))
$manifest = [Text.Encoding]::UTF8.GetString($manifestBytes) | ConvertFrom-Json
Assert-PlatformRelease ($manifest.schema_version -eq 1) "Unsupported Platform Release Manifest schema"
Assert-PlatformRelease ([int]$manifest.bootstrap_contract.minimum_schema -le 1 -and [int]$manifest.bootstrap_contract.maximum_schema -ge 1) "Platform Release is incompatible with this bootstrap planner"
Assert-PlatformRelease ($manifest.release.repository -eq $policy.repository) "Platform Release repository does not match pinned trust policy"
Assert-PlatformRelease ($manifest.provenance.repository -eq $policy.repository) "Platform Release provenance repository does not match pinned trust policy"
Assert-PlatformRelease ($manifest.provenance.workflow_path -eq $policy.workflow_path) "Platform Release workflow does not match pinned trust policy"
Assert-PlatformRelease ($manifest.signature.key_id -eq $policy.key_id) "Platform Release signing key does not match pinned trust policy"
Assert-PlatformRelease ($manifest.signature.algorithm -eq $policy.signature_algorithm) "Platform Release signature algorithm does not match pinned trust policy"
Assert-PlatformRelease ($manifest.signature.signature_asset -eq [IO.Path]::GetFileName($SignaturePath)) "Platform Release signature asset identity is invalid"

$releaseVersion = Get-SemanticVersion ([string]$manifest.release.version)
$minimumVersion = Get-SemanticVersion ([string]$policy.minimum_platform_version)
Assert-PlatformRelease ($releaseVersion -ge $minimumVersion) "Platform Release is older than the pinned minimum version"
Assert-PlatformRelease ($manifest.release.channel -eq "stable") "Bootstrap accepts stable Platform Releases only"
Assert-PlatformRelease ($manifest.release.tag -eq ("platform-v" + $releaseVersion.ToString(3))) "Platform Release tag does not match its version"
Assert-PlatformRelease (([string]$manifest.release.source_commit) -match '^[0-9a-f]{40}$') "Platform Release source commit is invalid"
Assert-PlatformRelease ([long]$manifest.release.github_actions_run_id -gt 0) "Platform Release GitHub Actions run identity is invalid"
Assert-PlatformRelease ($manifest.provenance.source_commit -eq $manifest.release.source_commit) "Platform Release provenance source commit does not match"
Assert-PlatformRelease ([long]$manifest.provenance.github_actions_run_id -eq [long]$manifest.release.github_actions_run_id) "Platform Release provenance run identity does not match"
Assert-PlatformRelease ($manifest.provenance.builder_id -eq "github-actions") "Platform Release provenance builder is invalid"

$artifactIdentities = @{}
foreach ($artifact in @($manifest.artifacts)) {
    $name = [string](Get-RequiredProperty $artifact "name")
    $sha = [string](Get-RequiredProperty $artifact "sha256")
    $size = [long](Get-RequiredProperty $artifact "size")
    Assert-PlatformRelease ($name -eq [IO.Path]::GetFileName($name)) "Platform Release artifact name is invalid"
    Assert-PlatformRelease ($sha -match '^[0-9a-f]{64}$') "Platform Release artifact checksum is invalid"
    Assert-PlatformRelease ($size -gt 0) "Platform Release artifact size is invalid"
    Assert-PlatformRelease (-not $artifactIdentities.ContainsKey($name)) "Platform Release artifact is duplicated"
    $artifactIdentities[$name] = "$sha`:$size"
}
foreach ($required in @("workflow-windows-amd64.zip", "platform-sbom.spdx.json", "platform-provenance.json")) {
    Assert-PlatformRelease ($artifactIdentities.ContainsKey($required)) "Platform Release lacks required artifact '$required'"
}
$subjectIdentities = @{}
foreach ($subject in @($manifest.provenance.subjects)) {
    $name = [string](Get-RequiredProperty $subject "name")
    $sha = [string](Get-RequiredProperty $subject "sha256")
    $size = [long](Get-RequiredProperty $subject "size")
    Assert-PlatformRelease (-not $subjectIdentities.ContainsKey($name)) "Platform Release provenance subject is duplicated"
    $subjectIdentities[$name] = "$sha`:$size"
}
Assert-PlatformRelease ($subjectIdentities.Count -eq $artifactIdentities.Count) "Platform Release provenance subjects do not exactly cover artifacts"
foreach ($name in $artifactIdentities.Keys) {
    Assert-PlatformRelease ($subjectIdentities[$name] -eq $artifactIdentities[$name]) "Platform Release provenance subject '$name' does not match"
}

$publicKeyPEM = Get-Content -LiteralPath $PublicKeyPath -Raw
$publicKey = Import-PinnedP256PublicKey $publicKeyPEM
try {
    Assert-PlatformRelease ($publicKey.KeySize -eq 256) "Pinned Platform Release public key is not ECDSA P-256"
    $signature = [IO.File]::ReadAllBytes([IO.Path]::GetFullPath($SignaturePath))
    $verified = $publicKey.VerifyData($manifestBytes, (Convert-DerEcdsaSignature $signature))
    Assert-PlatformRelease $verified "Platform Release signature is invalid"
} finally {
    $publicKey.Dispose()
}

$hasher = [Security.Cryptography.SHA256]::Create()
try { $digest = ([BitConverter]::ToString($hasher.ComputeHash($manifestBytes))).Replace("-", "").ToLowerInvariant() } finally { $hasher.Dispose() }
[ordered]@{
    verified = $true
    manifest_digest_sha256 = $digest
    release_version = [string]$manifest.release.version
    repository = [string]$manifest.release.repository
    source_commit = [string]$manifest.release.source_commit
    workflow_path = [string]$manifest.provenance.workflow_path
    github_actions_run_id = [long]$manifest.release.github_actions_run_id
} | ConvertTo-Json -Compress
