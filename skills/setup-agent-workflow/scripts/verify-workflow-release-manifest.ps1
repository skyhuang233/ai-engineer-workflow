[CmdletBinding()]
param(
  [Parameter(Mandatory)][string]$ManifestPath,
  [Parameter(Mandatory)][string]$ExpectedSHA256,
  [Parameter(Mandatory)][long]$ExpectedSize,
  [Parameter(Mandatory)][string]$ExpectedTag
)

$ErrorActionPreference = 'Stop'

function Assert-ExactObject {
  param(
    [Parameter(Mandatory)][System.Text.Json.JsonElement]$Element,
    [Parameter(Mandatory)][string]$Path,
    [Parameter(Mandatory)][string[]]$Names
  )
  if ($Element.ValueKind -ne [System.Text.Json.JsonValueKind]::Object) {
    throw "$Path must be a JSON object"
  }
  $allowed = [Collections.Generic.HashSet[string]]::new([StringComparer]::Ordinal)
  foreach ($name in $Names) { [void]$allowed.Add($name) }
  $seen = [Collections.Generic.HashSet[string]]::new([StringComparer]::Ordinal)
  foreach ($property in $Element.EnumerateObject()) {
    if (-not $seen.Add($property.Name)) { throw "$Path contains duplicate field '$($property.Name)'" }
    if (-not $allowed.Contains($property.Name)) { throw "$Path contains unknown field '$($property.Name)'" }
  }
  foreach ($name in $Names) {
    if (-not $seen.Contains($name)) { throw "$Path is missing required field '$name'" }
  }
}

function Get-RequiredProperty {
  param(
    [Parameter(Mandatory)][System.Text.Json.JsonElement]$Element,
    [Parameter(Mandatory)][string]$Name
  )
  return $Element.GetProperty($Name)
}

function Assert-String {
  param(
    [Parameter(Mandatory)][System.Text.Json.JsonElement]$Element,
    [Parameter(Mandatory)][string]$Path,
    [Parameter(Mandatory)][string]$Pattern
  )
  if ($Element.ValueKind -ne [System.Text.Json.JsonValueKind]::String) { throw "$Path must be a JSON string" }
  $value = $Element.GetString()
  if ($value -cnotmatch $Pattern) { throw "$Path does not match its schema" }
  return $value
}

function Assert-LiteralString {
  param(
    [Parameter(Mandatory)][System.Text.Json.JsonElement]$Element,
    [Parameter(Mandatory)][string]$Path,
    [Parameter(Mandatory)][string]$Expected
  )
  $value = Assert-String $Element $Path '^.+$'
  if ($value -cne $Expected) { throw "$Path must equal '$Expected'" }
  return $value
}

function Assert-NonWhitespaceString {
  param(
    [Parameter(Mandatory)][System.Text.Json.JsonElement]$Element,
    [Parameter(Mandatory)][string]$Path
  )
  if ($Element.ValueKind -ne [System.Text.Json.JsonValueKind]::String) { throw "$Path must be a JSON string" }
  $value = $Element.GetString()
  if ([string]::IsNullOrWhiteSpace($value)) { throw "$Path must not be empty or whitespace" }
  return $value
}

function Assert-PositiveInteger {
  param(
    [Parameter(Mandatory)][System.Text.Json.JsonElement]$Element,
    [Parameter(Mandatory)][string]$Path
  )
  if ($Element.ValueKind -ne [System.Text.Json.JsonValueKind]::Number) { throw "$Path must be a JSON integer" }
  $value = 0L
  if (-not $Element.TryGetInt64([ref]$value) -or $value -le 0) { throw "$Path must be a positive JSON integer" }
  return $value
}

$resolvedManifest = [IO.Path]::GetFullPath($ManifestPath)
if ((Split-Path -Leaf $resolvedManifest) -cne 'workflow-release.json') { throw 'Unexpected Workflow Release manifest asset name' }
if ($ExpectedSHA256 -cnotmatch '^sha256:[0-9a-f]{64}$') { throw 'Manifest asset metadata lacks a valid sha256 digest' }
$expectedDigest = $ExpectedSHA256.Substring('sha256:'.Length)
$actualDigest = (Get-FileHash -LiteralPath $resolvedManifest -Algorithm SHA256).Hash.ToLowerInvariant()
if ($actualDigest -cne $expectedDigest) { throw 'Manifest SHA-256 does not match immutable Release metadata' }
if ($ExpectedSize -le 0 -or (Get-Item -LiteralPath $resolvedManifest).Length -ne $ExpectedSize) { throw 'Manifest size does not match immutable Release metadata' }
if ($ExpectedTag -cnotmatch '^workflow-v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$') { throw 'Unexpected Workflow Release tag' }
$expectedVersion = $ExpectedTag.Substring('workflow-v'.Length)

$utf8 = [Text.UTF8Encoding]::new($false, $true)
$json = [IO.File]::ReadAllText($resolvedManifest, $utf8)
$document = [System.Text.Json.JsonDocument]::Parse($json)
try {
  $root = $document.RootElement
  Assert-ExactObject $root '$' @('schema_version','version','candidate_source_commit','qualification_run_id','qualification_run_attempt','bundle','worker','sbom')

  $schema = Get-RequiredProperty $root 'schema_version'
  if ($schema.ValueKind -ne [System.Text.Json.JsonValueKind]::Number -or $schema.GetRawText() -cne '1') { throw '$.schema_version must equal integer 1' }
  $version = Assert-String (Get-RequiredProperty $root 'version') '$.version' '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'
  foreach ($component in $version.Split('.')) {
    $parsedComponent = 0L
    if (-not [int64]::TryParse($component, [Globalization.NumberStyles]::None, [Globalization.CultureInfo]::InvariantCulture, [ref]$parsedComponent) -or $parsedComponent -gt 2147483647) {
      throw '$.version components must fit signed 32-bit range'
    }
  }
  if ($version -cne $expectedVersion) { throw '$.version does not match the Release tag' }
  $sourceCommit = Assert-String (Get-RequiredProperty $root 'candidate_source_commit') '$.candidate_source_commit' '^[0-9a-f]{40}$'
  $runID = Assert-PositiveInteger (Get-RequiredProperty $root 'qualification_run_id') '$.qualification_run_id'
  $runAttempt = Assert-PositiveInteger (Get-RequiredProperty $root 'qualification_run_attempt') '$.qualification_run_attempt'

  $bundle = Get-RequiredProperty $root 'bundle'
  Assert-ExactObject $bundle '$.bundle' @('name','sha256')
  [void](Assert-LiteralString (Get-RequiredProperty $bundle 'name') '$.bundle.name' 'workflow-windows-amd64.zip')
  $bundleDigest = Assert-String (Get-RequiredProperty $bundle 'sha256') '$.bundle.sha256' '^[0-9a-f]{64}$'

  $worker = Get-RequiredProperty $root 'worker'
  Assert-ExactObject $worker '$.worker' @('image','tools')
  $workerImage = Assert-String (Get-RequiredProperty $worker 'image') '$.worker.image' '^ghcr\.io/skyhuang233/workflow-worker@sha256:[0-9a-f]{64}$'

  $tools = Get-RequiredProperty $worker 'tools'
  Assert-ExactObject $tools '$.worker.tools' @('codex','github_cli','go','no_mistakes')
  $codex = Get-RequiredProperty $tools 'codex'
  Assert-ExactObject $codex '$.worker.tools.codex' @('version')
  [void](Assert-NonWhitespaceString (Get-RequiredProperty $codex 'version') '$.worker.tools.codex.version')
  foreach ($toolName in @('github_cli','go')) {
    $tool = Get-RequiredProperty $tools $toolName
    Assert-ExactObject $tool "$.worker.tools.$toolName" @('version','linux_amd64_sha256')
    [void](Assert-NonWhitespaceString (Get-RequiredProperty $tool 'version') "$.worker.tools.$toolName.version")
    [void](Assert-String (Get-RequiredProperty $tool 'linux_amd64_sha256') "$.worker.tools.$toolName.linux_amd64_sha256" '^[0-9a-f]{64}$')
  }
  $noMistakes = Get-RequiredProperty $tools 'no_mistakes'
  Assert-ExactObject $noMistakes '$.worker.tools.no_mistakes' @('version','repository','commit')
  [void](Assert-NonWhitespaceString (Get-RequiredProperty $noMistakes 'version') '$.worker.tools.no_mistakes.version')
  [void](Assert-String (Get-RequiredProperty $noMistakes 'repository') '$.worker.tools.no_mistakes.repository' '^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$')
  [void](Assert-String (Get-RequiredProperty $noMistakes 'commit') '$.worker.tools.no_mistakes.commit' '^[0-9a-f]{40}$')

  $sbom = Get-RequiredProperty $root 'sbom'
  Assert-ExactObject $sbom '$.sbom' @('name','format','sha256','scan')
  [void](Assert-LiteralString (Get-RequiredProperty $sbom 'name') '$.sbom.name' 'worker-sbom.spdx.json')
  [void](Assert-LiteralString (Get-RequiredProperty $sbom 'format') '$.sbom.format' 'spdx-json')
  $sbomDigest = Assert-String (Get-RequiredProperty $sbom 'sha256') '$.sbom.sha256' '^[0-9a-f]{64}$'
  $scan = Get-RequiredProperty $sbom 'scan'
  Assert-ExactObject $scan '$.sbom.scan' @('scanner','severity_cutoff','only_fixed')
  [void](Assert-LiteralString (Get-RequiredProperty $scan 'scanner') '$.sbom.scan.scanner' 'grype')
  [void](Assert-LiteralString (Get-RequiredProperty $scan 'severity_cutoff') '$.sbom.scan.severity_cutoff' 'high')
  $onlyFixed = Get-RequiredProperty $scan 'only_fixed'
  if ($onlyFixed.ValueKind -ne [System.Text.Json.JsonValueKind]::True) { throw '$.sbom.scan.only_fixed must equal true' }

  [pscustomobject]@{
    schema_version = 1
    version = $version
    candidate_source_commit = $sourceCommit
    qualification_run_id = $runID
    qualification_run_attempt = $runAttempt
    bundle_sha256 = $bundleDigest
    worker_image = $workerImage
    sbom_sha256 = $sbomDigest
    manifest_sha256 = $actualDigest
  } | ConvertTo-Json -Compress
} finally {
  $document.Dispose()
}
