[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$ManifestPath,
    [Parameter(Mandatory = $true)][string]$HostFactsPath,
    [Parameter(Mandatory = $true)][string]$OutputPath,
    [string]$GitHubOwner = ""
)

$ErrorActionPreference = "Stop"
$manifest = Get-Content -LiteralPath $ManifestPath -Raw | ConvertFrom-Json
$facts = Get-Content -LiteralPath $HostFactsPath -Raw | ConvertFrom-Json
if ($manifest.schema_version -ne 1) { throw "Unsupported Platform Release Manifest schema" }
if ($facts.schema_version -ne 1) { throw "Unsupported host-facts schema" }
if (-not $facts.supported_host) { throw "Agent Workflow setup supports Windows only" }

$actions = [System.Collections.Generic.List[object]]::new()
$manifestDigest = (Get-FileHash -LiteralPath $ManifestPath -Algorithm SHA256).Hash.ToLowerInvariant()
if (-not $facts.workflow.installed) {
    $actions.Add([ordered]@{ id = "install-platform-cli"; kind = "platform_cli"; subject = (Join-Path $facts.workflow_home "bin\workflow.exe"); action = "install"; parameters = [ordered]@{ version = $manifest.release.version } })
}
if (-not $facts.docker.installed) {
    $actions.Add([ordered]@{ id = "install-docker-desktop"; kind = "docker_desktop"; subject = "current-host"; action = "install"; parameters = [ordered]@{ version = $manifest.platform_setup_contract.docker_desktop.version } })
}
if (-not $facts.github_credential.exists) {
    if ([string]::IsNullOrWhiteSpace($GitHubOwner)) { throw "GitHubOwner is required when the Control Plane PAT is not persisted" }
    $actions.Add([ordered]@{ id = "persist-classic-pat"; kind = "github_pat"; subject = $facts.github_credential.path; action = "persist"; parameters = [ordered]@{ input = "stdin"; owner = $GitHubOwner } })
}
$actions.Add([ordered]@{ id = "record-platform-installation"; kind = "platform_installation"; subject = $facts.workflow_home; action = "record"; parameters = [ordered]@{ version = $manifest.release.version; release_manifest_digest = $manifestDigest } })

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
$identityDigest = [Convert]::ToHexString([Security.Cryptography.SHA256]::HashData($identityBytes)).ToLowerInvariant()
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
$digest = [Convert]::ToHexString([Security.Cryptography.SHA256]::HashData($bytes)).ToLowerInvariant()
$envelope = [ordered]@{ status = $(if ($actions.Count -eq 0) { "ready" } else { "plan_required" }); digest_sha256 = $digest; plan = $seed; projection = ($seed | ConvertTo-Json -Depth 10) }
$directory = Split-Path -Parent ([System.IO.Path]::GetFullPath($OutputPath))
if ($directory) { New-Item -ItemType Directory -Path $directory -Force | Out-Null }
$temporary = Join-Path $directory ("." + [System.IO.Path]::GetFileName($OutputPath) + "." + [Guid]::NewGuid().ToString("N") + ".tmp")
$envelope | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $temporary -Encoding utf8NoBOM
Move-Item -LiteralPath $temporary -Destination $OutputPath -Force
$envelope | ConvertTo-Json -Depth 12
