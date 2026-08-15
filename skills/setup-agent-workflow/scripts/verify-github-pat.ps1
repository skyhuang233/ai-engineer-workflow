[CmdletBinding()]
param(
    [string]$APIBase = "https://api.github.com",
    [Parameter(Mandatory = $true)][string]$Owner
)

$ErrorActionPreference = "Stop"
$token = [Console]::In.ReadToEnd().Trim()
if ([string]::IsNullOrWhiteSpace($token)) { throw "A classic GitHub PAT is required on standard input" }
try {
    $response = Invoke-WebRequest -Uri ($APIBase.TrimEnd('/') + "/user") -Headers @{
        Authorization = "Bearer $token"
        Accept = "application/vnd.github+json"
        "X-GitHub-Api-Version" = "2022-11-28"
    } -UseBasicParsing
} catch {
    throw "GitHub rejected the Control Plane credential or requires SSO authorization"
}
$scopes = @($response.Headers["X-OAuth-Scopes"] -split ',' | ForEach-Object { $_.Trim().ToLowerInvariant() } | Where-Object { $_ })
if ($scopes -notcontains "repo" -or $scopes -notcontains "workflow") { throw "The classic PAT requires repo and workflow scopes" }
$identity = $response.Content | ConvertFrom-Json
if ([string]::IsNullOrWhiteSpace([string]$identity.login) -or [long]$identity.id -le 0) { throw "GitHub returned an invalid credential identity" }
$boundOwner = $Owner.Trim()
if ([string]::IsNullOrWhiteSpace($boundOwner)) { throw "A personal account or organization owner is required before Platform planning" }
if (-not [string]::Equals($boundOwner, [string]$identity.login, [StringComparison]::OrdinalIgnoreCase)) {
    try {
        $membershipResponse = Invoke-WebRequest -Uri ($APIBase.TrimEnd('/') + "/orgs/" + [Uri]::EscapeDataString($boundOwner) + "/memberships/" + [Uri]::EscapeDataString([string]$identity.login)) -Headers @{
            Authorization = "Bearer $token"
            Accept = "application/vnd.github+json"
            "X-GitHub-Api-Version" = "2022-11-28"
        } -UseBasicParsing
    } catch {
        throw "GitHub rejected the requested owner binding, lacks organization administration, or requires SSO authorization"
    }
    $membership = $membershipResponse.Content | ConvertFrom-Json
    if ([string]$membership.state -ne "active" -or [string]$membership.role -ne "admin") { throw "The classic PAT identity must be an active organization owner" }
}
$hasher = [Security.Cryptography.SHA256]::Create()
try { $digest = ([BitConverter]::ToString($hasher.ComputeHash([Text.Encoding]::UTF8.GetBytes($token)))).Replace("-", "").ToLowerInvariant() } finally { $hasher.Dispose() }
[ordered]@{ login = [string]$identity.login; user_id = [long]$identity.id; owner = $boundOwner; scopes = $scopes; fingerprint_sha256 = $digest } | ConvertTo-Json -Depth 4
