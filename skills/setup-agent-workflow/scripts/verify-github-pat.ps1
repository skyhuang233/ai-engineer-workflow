[CmdletBinding()]
param([string]$APIBase = "https://api.github.com")

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
$digest = [Convert]::ToHexString([Security.Cryptography.SHA256]::HashData([Text.Encoding]::UTF8.GetBytes($token))).ToLowerInvariant()
[ordered]@{ login = $identity.login; user_id = $identity.id; scopes = $scopes; fingerprint_sha256 = $digest } | ConvertTo-Json -Depth 4
