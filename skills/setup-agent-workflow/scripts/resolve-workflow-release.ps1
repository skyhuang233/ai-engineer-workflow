[CmdletBinding()]
param(
  [Parameter(Mandatory)][string]$DownloadDirectory
)

$ErrorActionPreference = 'Stop'
$policyPath = Join-Path (Split-Path $PSScriptRoot -Parent) 'trust\release-policy.json'
$policy = Get-Content -LiteralPath $policyPath -Raw | ConvertFrom-Json
$repository = [string]$policy.repository
if ([int]$policy.schema_version -ne 1) { throw 'Release policy schema is invalid' }
if ($repository -cnotmatch '^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$') { throw 'Release policy repository is invalid' }
if ([string]$policy.workflow_path -cne '.github/workflows/publish-workflow.yml') { throw 'Release policy publisher path is invalid' }
$minimumMatch = [regex]::Match([string]$policy.minimum_workflow_version, '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$')
if (-not $minimumMatch.Success) { throw 'Release policy minimum Workflow version is invalid' }
$minimumComponents = [Collections.Generic.List[int64]]::new()
foreach ($index in 1..3) {
  $component = 0L
  if (-not [int64]::TryParse($minimumMatch.Groups[$index].Value, [Globalization.NumberStyles]::None, [Globalization.CultureInfo]::InvariantCulture, [ref]$component) -or $component -gt 2147483647) {
    throw 'Release policy minimum Workflow version is invalid'
  }
  $minimumComponents.Add($component)
}

function Invoke-GhJSON {
  param([Parameter(Mandatory)][string]$Endpoint)
  $output = & gh api $Endpoint 2>&1
  if ($LASTEXITCODE -ne 0) { throw "GitHub API request failed for $Endpoint`: $output" }
  return ($output | Out-String | ConvertFrom-Json)
}

function Invoke-GhPagedJSON {
  param([Parameter(Mandatory)][string]$Endpoint)
  $output = & gh api --paginate --slurp $Endpoint 2>&1
  if ($LASTEXITCODE -ne 0) { throw "GitHub API request failed for $Endpoint`: $output" }
  $pages = @($output | Out-String | ConvertFrom-Json)
  $items = [Collections.Generic.List[object]]::new()
  foreach ($page in $pages) {
    foreach ($item in @($page)) { $items.Add($item) }
  }
  return $items
}

function Get-Asset {
  param([Parameter(Mandatory)]$Release, [Parameter(Mandatory)][string]$Name)
  $matches = @($Release.assets | Where-Object { [string]$_.name -ceq $Name })
  if ($matches.Count -ne 1) { throw "Release requires exactly one $Name asset" }
  $asset = $matches[0]
  if ([string]$asset.state -cne 'uploaded' -or [long]$asset.size -le 0) { throw "Release asset $Name is not completely uploaded" }
  if ([string]$asset.digest -cnotmatch '^sha256:[0-9a-f]{64}$') { throw "Release asset $Name lacks immutable SHA-256 metadata" }
  return $asset
}

function Download-ReleaseAsset {
  param([Parameter(Mandatory)][string]$Tag, [Parameter(Mandatory)][string]$Name)
  & gh release download $Tag --repo $repository --pattern $Name --dir $resolvedDownload --clobber
  if ($LASTEXITCODE -ne 0) { throw "Cannot download Workflow Release asset $Name" }
  $path = Join-Path $resolvedDownload $Name
  if (-not (Test-Path -LiteralPath $path -PathType Leaf)) { throw "Downloaded Workflow Release asset $Name is absent" }
  return $path
}

function Assert-DownloadedDigest {
  param([Parameter(Mandatory)][string]$Path, [Parameter(Mandatory)][string]$Expected, [Parameter(Mandatory)][long]$ExpectedSize, [Parameter(Mandatory)][string]$Label)
  $expectedHex = $Expected -replace '^sha256:', ''
  $actualHex = (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant()
  if ($actualHex -cne $expectedHex) { throw "$Label SHA-256 does not match immutable Release metadata" }
  if ($ExpectedSize -le 0 -or (Get-Item -LiteralPath $Path).Length -ne $ExpectedSize) { throw "$Label size does not match immutable Release metadata" }
  return $actualHex
}

function Get-GitHubTimestamp {
  param([Parameter(Mandatory)]$Value, [Parameter(Mandatory)][string]$Label)
  if ($Value -is [DateTimeOffset]) {
    return $Value.ToUniversalTime().ToString("yyyy-MM-dd'T'HH:mm:ss'Z'", [Globalization.CultureInfo]::InvariantCulture)
  }
  if ($Value -is [DateTime]) {
    return ([DateTimeOffset]$Value).ToUniversalTime().ToString("yyyy-MM-dd'T'HH:mm:ss'Z'", [Globalization.CultureInfo]::InvariantCulture)
  }
  $text = [string]$Value
  if ($text -cnotmatch '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$') { throw "$Label timestamp is invalid" }
  return $text
}

$resolvedDownload = [IO.Path]::GetFullPath($DownloadDirectory)
New-Item -ItemType Directory -Force -Path $resolvedDownload | Out-Null
if (@(Get-ChildItem -LiteralPath $resolvedDownload -Force).Count -ne 0) { throw 'Workflow Release download directory must be empty' }

$releases = @(Invoke-GhPagedJSON "repos/$repository/releases?per_page=100")
$eligible = [Collections.Generic.List[object]]::new()
foreach ($release in $releases) {
  $match = [regex]::Match([string]$release.tag_name, '^workflow-v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$')
  if (-not $match.Success) { continue }
  if ([bool]$release.draft -or [bool]$release.prerelease -or -not [bool]$release.immutable -or [string]::IsNullOrWhiteSpace([string]$release.published_at) -or
      [string]$release.author.login -cne 'github-actions[bot]' -or [string]$release.author.type -cne 'Bot') { continue }
  $components = [Collections.Generic.List[int64]]::new()
  $validComponents = $true
  foreach ($index in 1..3) {
    $component = 0L
    if (-not [int64]::TryParse($match.Groups[$index].Value, [Globalization.NumberStyles]::None, [Globalization.CultureInfo]::InvariantCulture, [ref]$component) -or $component -gt 2147483647) {
      $validComponents = $false
      break
    }
    $components.Add($component)
  }
  if (-not $validComponents) { continue }
  $belowMinimum = $false
  foreach ($index in 0..2) {
    if ($components[$index] -lt $minimumComponents[$index]) { $belowMinimum = $true; break }
    if ($components[$index] -gt $minimumComponents[$index]) { break }
  }
  if ($belowMinimum) { continue }
  $eligible.Add([pscustomobject]@{Release=$release; Major=$components[0]; Minor=$components[1]; Patch=$components[2]})
}
if ($eligible.Count -eq 0) { throw 'No published immutable Workflow Release is available' }
$selected = $eligible | Sort-Object -Property @{Expression='Major';Descending=$true},@{Expression='Minor';Descending=$true},@{Expression='Patch';Descending=$true} | Select-Object -First 1
$release = $selected.Release
$expectedNames = @('workflow-windows-amd64.zip','workflow-release.json','worker-sbom.spdx.json')
if (@($release.assets).Count -ne $expectedNames.Count) { throw 'Workflow Release must contain exactly three assets' }
foreach ($asset in @($release.assets)) {
  if ([string]$asset.name -cnotin $expectedNames) { throw "Workflow Release contains unexpected asset $($asset.name)" }
}
$bundleAsset = Get-Asset $release 'workflow-windows-amd64.zip'
$manifestAsset = Get-Asset $release 'workflow-release.json'
$sbomAsset = Get-Asset $release 'worker-sbom.spdx.json'

# Authenticate and validate only the manifest before acquiring any other asset.
$manifestPath = Download-ReleaseAsset ([string]$release.tag_name) 'workflow-release.json'
$validator = Join-Path $PSScriptRoot 'verify-workflow-release-manifest.ps1'
$validatedJSON = & $validator -ManifestPath $manifestPath -ExpectedSHA256 ([string]$manifestAsset.digest) -ExpectedSize ([long]$manifestAsset.size) -ExpectedTag ([string]$release.tag_name)
if ($LASTEXITCODE -ne 0) { throw 'Workflow Release manifest bootstrap validation failed' }
$validated = $validatedJSON | ConvertFrom-Json
$tagRef = Invoke-GhJSON "repos/$repository/git/ref/tags/$([Uri]::EscapeDataString([string]$release.tag_name))"
if ([string]$tagRef.object.type -cne 'tag' -or [string]$tagRef.object.sha -cnotmatch '^[0-9a-f]{40}$') { throw 'Workflow Release tag lacks annotated publisher provenance' }
$tagObject = Invoke-GhJSON "repos/$repository/git/tags/$([string]$tagRef.object.sha)"
$provenanceMatch = [regex]::Match([string]$tagObject.message, '^Workflow publisher provenance\nrun_id=([1-9][0-9]*)\nrun_attempt=([1-9][0-9]*)$')
if (-not $provenanceMatch.Success -or [string]$tagObject.tag -cne [string]$release.tag_name -or [string]$tagObject.object.type -cne 'commit' -or [string]$tagObject.object.sha -cnotmatch '^[0-9a-f]{40}$') {
  throw 'Workflow Release tag provenance does not bind its publisher merge commit'
}
$mergeCommit = [string]$tagObject.object.sha
$publisherRunID = [long]$provenanceMatch.Groups[1].Value
$publisherRunAttempt = [long]$provenanceMatch.Groups[2].Value
$expectedReleaseBody = "Immutable atomic Agent Workflow release.`n`nPublisher Run: $publisherRunID`nPublisher Attempt: $publisherRunAttempt"
if ([string]$release.body -cne $expectedReleaseBody) { throw 'Workflow Release body differs from its annotated publisher provenance' }

$pullSummaries = @(Invoke-GhJSON "repos/$repository/commits/$mergeCommit/pulls")
$matchingPulls = @($pullSummaries | Where-Object {
  -not [string]::IsNullOrWhiteSpace([string]$_.merged_at) -and
  [string]$_.merge_commit_sha -ceq $mergeCommit -and [string]$_.base.ref -ceq 'main'
})
if ($matchingPulls.Count -ne 1 -or [long]$matchingPulls[0].number -le 0) { throw 'Workflow Release publisher merge lacks one merged main Pull Request' }
$pull = Invoke-GhJSON "repos/$repository/pulls/$([long]$matchingPulls[0].number)"
$candidateCommit = [string]$validated.candidate_source_commit
$owner = $repository.Split('/')[0]
$expectedBranch = "release-$($validated.version)"
$expectedHotfix = "hotfix-$($validated.version)"
$mergedAt = Get-GitHubTimestamp $pull.merged_at 'Workflow Release owner merge'
if ([string]$pull.merge_commit_sha -cne $mergeCommit -or [string]$pull.base.ref -cne 'main') { throw 'Workflow Release publisher target is not the Pull Request main merge' }
if ([string]$pull.head.ref -cne $expectedBranch -and [string]$pull.head.ref -cne $expectedHotfix) { throw 'Workflow Release publisher branch does not match its version' }
if ([string]$pull.head.sha -cne $candidateCommit) { throw 'Workflow Release owner merge does not contain the qualified candidate head' }
if (-not [string]::Equals([string]$pull.merged_by.login, $owner, [StringComparison]::OrdinalIgnoreCase) -or
    -not [string]::Equals([string]$pull.merged_by.type, 'user', [StringComparison]::OrdinalIgnoreCase) -or
    [string]$pull.merged_by.login -imatch '\[bot\]$') { throw 'Workflow Release publisher merge was not performed by the repository owner' }
$integration = Invoke-GhJSON "repos/$repository/git/commits/$mergeCommit"
$parents = @($integration.parents)
if ($parents.Count -ne 2 -or @($parents | Where-Object { [string]$_.sha -ceq $candidateCommit }).Count -ne 1) {
  throw 'Workflow Release publisher commit is not a two-parent merge containing the qualified candidate'
}

$qualificationWorkflow = Invoke-GhJSON "repos/$repository/actions/workflows/worker-contract.yml"
$qualificationRun = Invoke-GhJSON "repos/$repository/actions/runs/$($validated.qualification_run_id)/attempts/$($validated.qualification_run_attempt)"
if ([string]$qualificationWorkflow.path -cne '.github/workflows/worker-contract.yml' -or [string]$qualificationWorkflow.state -cne 'active' -or
    [long]$qualificationRun.workflow_id -ne [long]$qualificationWorkflow.id -or [string]$qualificationRun.path -cne '.github/workflows/worker-contract.yml' -or
    [long]$qualificationRun.run_attempt -ne [long]$validated.qualification_run_attempt -or
    [string]$qualificationRun.head_sha -cne $candidateCommit -or [string]$qualificationRun.event -cne 'pull_request' -or
    [string]$qualificationRun.status -cne 'completed' -or [string]$qualificationRun.conclusion -cne 'success' -or
    @($qualificationRun.pull_requests | Where-Object { [long]$_.number -eq [long]$pull.number }).Count -eq 0) {
  throw 'Qualified candidate was not produced by the authoritative successful qualification run for its Pull Request'
}
$qualificationCompletedAt = Get-GitHubTimestamp $qualificationRun.updated_at 'Workflow Release qualification completion'
if ([DateTimeOffset]::Parse($qualificationCompletedAt, [Globalization.CultureInfo]::InvariantCulture) -gt [DateTimeOffset]::Parse($mergedAt, [Globalization.CultureInfo]::InvariantCulture)) {
  throw 'Qualification did not complete before the owner merge'
}

$publisherWorkflowName = Split-Path -Leaf ([string]$policy.workflow_path)
$publisherWorkflow = Invoke-GhJSON "repos/$repository/actions/workflows/$publisherWorkflowName"
$publisherRun = Invoke-GhJSON "repos/$repository/actions/runs/$publisherRunID"
if ([string]$publisherWorkflow.path -cne [string]$policy.workflow_path -or [string]$publisherWorkflow.state -cne 'active' -or
    [long]$publisherRun.id -ne $publisherRunID -or [long]$publisherRun.run_attempt -lt $publisherRunAttempt -or
    [long]$publisherRun.workflow_id -ne [long]$publisherWorkflow.id -or [string]$publisherRun.path -cne [string]$policy.workflow_path -or
    [string]$publisherRun.head_sha -cne $mergeCommit -or [string]$publisherRun.head_branch -cne 'main' -or [string]$publisherRun.event -cne 'push' -or
    [string]$publisherRun.status -cne 'completed' -or [string]$publisherRun.conclusion -cne 'success') {
  throw 'Workflow Release provenance is not its exact trusted successful publisher run'
}

$bundlePath = Download-ReleaseAsset ([string]$release.tag_name) 'workflow-windows-amd64.zip'
$sbomPath = Download-ReleaseAsset ([string]$release.tag_name) 'worker-sbom.spdx.json'
$bundleMetadataDigest = Assert-DownloadedDigest $bundlePath ([string]$bundleAsset.digest) ([long]$bundleAsset.size) 'Bundle'
$sbomMetadataDigest = Assert-DownloadedDigest $sbomPath ([string]$sbomAsset.digest) ([long]$sbomAsset.size) 'Worker SBOM'
if ($bundleMetadataDigest -cne [string]$validated.bundle_sha256) { throw 'Bundle digest differs between Release metadata and Workflow Release manifest' }
if ($sbomMetadataDigest -cne [string]$validated.sbom_sha256) { throw 'Worker SBOM digest differs between Release metadata and Workflow Release manifest' }

[pscustomobject]@{
  schema_version = 1
  version = [string]$validated.version
  tag = [string]$release.tag_name
  source_commit = $candidateCommit
  qualification_run_id = [long]$validated.qualification_run_id
  qualification_run_attempt = [long]$validated.qualification_run_attempt
  qualification_completed_at = $qualificationCompletedAt
  merged_at = $mergedAt
  publisher_source_commit = $mergeCommit
  publisher_run_id = $publisherRunID
  publisher_run_attempt = $publisherRunAttempt
  manifest_path = $manifestPath
  manifest_sha256 = [string]$validated.manifest_sha256
  bundle_path = $bundlePath
  bundle_sha256 = $bundleMetadataDigest
  sbom_path = $sbomPath
  sbom_sha256 = $sbomMetadataDigest
  worker_image = [string]$validated.worker_image
} | ConvertTo-Json -Compress
