[CmdletBinding()]
param(
    [string]$APIBase = "https://api.github.com",
    [Parameter(Mandatory = $true)][string]$Owner,
    [Parameter(Mandatory = $true)][string]$RepositoryName,
    [Parameter(Mandatory = $true)][ValidateSet("private", "public")][string]$Visibility,
    [Parameter(Mandatory = $true)][ValidateSet("published", "unpublished")][string]$PublicationState
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
$ownerType = "personal"
if ([string]::IsNullOrWhiteSpace($boundOwner)) { throw "A personal account or organization owner is required before Platform planning" }
if (-not [string]::Equals($boundOwner, [string]$identity.login, [StringComparison]::OrdinalIgnoreCase)) {
    $ownerType = "organization"
    if ($scopes -notcontains "admin:org") { throw "An organization-owned repository requires the classic PAT scopes repo, workflow, and admin:org" }
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
$repositoryNameValue = $RepositoryName.Trim()
if ([string]::IsNullOrWhiteSpace($repositoryNameValue) -or $repositoryNameValue.Contains('/')) { throw "The GitHub repository name is invalid" }
$repositoryID = $boundOwner + "/" + $repositoryNameValue
$repositoryExists = $false
try {
    $repositoryResponse = Invoke-WebRequest -Uri ($APIBase.TrimEnd('/') + "/repos/" + [Uri]::EscapeDataString($boundOwner) + "/" + [Uri]::EscapeDataString($repositoryNameValue)) -Headers @{
        Authorization = "Bearer $token"
        Accept = "application/vnd.github+json"
        "X-GitHub-Api-Version" = "2022-11-28"
    } -UseBasicParsing
    $repositoryExists = $true
} catch {
    $statusCode = 0
    if ($null -ne $_.Exception.Response) { $statusCode = [int]$_.Exception.Response.StatusCode }
    if ($statusCode -ne 404) { throw "GitHub repository intent cannot be proved or requires SSO authorization" }
}
if ($PublicationState -eq "published") {
    if (-not $repositoryExists) { throw "The confirmed published GitHub repository is unavailable" }
    $repository = $repositoryResponse.Content | ConvertFrom-Json
    if (-not [string]::Equals([string]$repository.full_name, $repositoryID, [StringComparison]::OrdinalIgnoreCase)) { throw "GitHub returned a different repository owner or name" }
    if ([bool]$repository.private -ne ($Visibility -eq "private")) { throw "GitHub repository visibility differs from the confirmed intent" }
    if (-not [bool]$repository.permissions.admin) { throw "The classic PAT identity lacks repository administration" }
    try {
        if ([string]::IsNullOrWhiteSpace([string]$repository.default_branch)) { throw "GitHub repository has no default branch" }
        if (-not [bool]$repository.allow_squash_merge -and -not [bool]$repository.allow_merge_commit -and -not [bool]$repository.allow_rebase_merge) { throw "GitHub repository has no supported merge method" }
        $repositoryActionsResponse = Invoke-WebRequest -Uri ($APIBase.TrimEnd('/') + "/repos/" + [Uri]::EscapeDataString($boundOwner) + "/" + [Uri]::EscapeDataString($repositoryNameValue) + "/actions/permissions") -Headers @{ Authorization = "Bearer $token"; Accept = "application/vnd.github+json"; "X-GitHub-Api-Version" = "2022-11-28" } -UseBasicParsing
        $repositoryActions = $repositoryActionsResponse.Content | ConvertFrom-Json
        if ([string]$repositoryActions.allowed_actions -eq "local_only") { throw "Repository Actions policy forbids the GitHub-owned checkout action" }
        if ([string]$repositoryActions.allowed_actions -eq "selected") {
            $repositorySelectedResponse = Invoke-WebRequest -Uri ($APIBase.TrimEnd('/') + "/repos/" + [Uri]::EscapeDataString($boundOwner) + "/" + [Uri]::EscapeDataString($repositoryNameValue) + "/actions/permissions/selected-actions") -Headers @{ Authorization = "Bearer $token"; Accept = "application/vnd.github+json"; "X-GitHub-Api-Version" = "2022-11-28" } -UseBasicParsing
            $repositorySelected = $repositorySelectedResponse.Content | ConvertFrom-Json
            if (-not [bool]$repositorySelected.github_owned_allowed) { throw "Repository Actions policy does not allow the GitHub-owned checkout action" }
        } elseif ([string]$repositoryActions.allowed_actions -ne "all") { throw "Repository Actions policy is unavailable" }
        $repositoryRulesetResponse = Invoke-WebRequest -Uri ($APIBase.TrimEnd('/') + "/repos/" + [Uri]::EscapeDataString($boundOwner) + "/" + [Uri]::EscapeDataString($repositoryNameValue) + "/rulesets?includes_parents=true") -Headers @{ Authorization = "Bearer $token"; Accept = "application/vnd.github+json"; "X-GitHub-Api-Version" = "2022-11-28" } -UseBasicParsing
        foreach ($ruleset in @($repositoryRulesetResponse.Content | ConvertFrom-Json)) {
            if ([string]$ruleset.enforcement -ne "active") { continue }
            foreach ($rule in @($ruleset.rules)) {
                if ([string]$rule.type -eq "merge_queue") { throw "Repository policy requires an unsupported merge queue" }
                if ([string]$rule.type -eq "pull_request" -and [int]$rule.parameters.required_approving_review_count -gt 0) { throw "Repository policy requires human review before onboarding" }
            }
        }
        try {
            $protectionResponse = Invoke-WebRequest -Uri ($APIBase.TrimEnd('/') + "/repos/" + [Uri]::EscapeDataString($boundOwner) + "/" + [Uri]::EscapeDataString($repositoryNameValue) + "/branches/" + [Uri]::EscapeDataString([string]$repository.default_branch) + "/protection") -Headers @{ Authorization = "Bearer $token"; Accept = "application/vnd.github+json"; "X-GitHub-Api-Version" = "2022-11-28" } -UseBasicParsing
            $protection = $protectionResponse.Content | ConvertFrom-Json
            if ([int]$protection.required_pull_request_reviews.required_approving_review_count -gt 0) { throw "Repository branch protection requires human review before onboarding" }
        } catch {
            $protectionStatus = 0
            if ($null -ne $_.Exception.Response) { $protectionStatus = [int]$_.Exception.Response.StatusCode }
            if ($protectionStatus -ne 404) { throw }
        }
    } catch {
        throw "Published repository policy cannot be proved before Platform mutation: $($_.Exception.Message)"
    }
} else {
    if ($repositoryExists) { throw "The confirmed unpublished GitHub repository already exists" }
    if (-not [string]::Equals($boundOwner, [string]$identity.login, [StringComparison]::OrdinalIgnoreCase)) {
        try {
            $organizationResponse = Invoke-WebRequest -Uri ($APIBase.TrimEnd('/') + "/orgs/" + [Uri]::EscapeDataString($boundOwner)) -Headers @{ Authorization = "Bearer $token"; Accept = "application/vnd.github+json"; "X-GitHub-Api-Version" = "2022-11-28" } -UseBasicParsing
            $organization = $organizationResponse.Content | ConvertFrom-Json
            if (-not [string]::Equals([string]$organization.login, $boundOwner, [StringComparison]::OrdinalIgnoreCase)) { throw "Organization policy returned a different owner" }
            $actionsResponse = Invoke-WebRequest -Uri ($APIBase.TrimEnd('/') + "/orgs/" + [Uri]::EscapeDataString($boundOwner) + "/actions/permissions") -Headers @{ Authorization = "Bearer $token"; Accept = "application/vnd.github+json"; "X-GitHub-Api-Version" = "2022-11-28" } -UseBasicParsing
            $actions = $actionsResponse.Content | ConvertFrom-Json
            if ([string]$actions.enabled_repositories -ne "all") { throw "Organization Actions policy does not prove new-repository enablement" }
            if ([string]$actions.allowed_actions -eq "local_only") { throw "Organization Actions policy forbids the GitHub-owned checkout action" }
            if ([string]$actions.allowed_actions -eq "selected") {
                $selectedResponse = Invoke-WebRequest -Uri ($APIBase.TrimEnd('/') + "/orgs/" + [Uri]::EscapeDataString($boundOwner) + "/actions/permissions/selected-actions") -Headers @{ Authorization = "Bearer $token"; Accept = "application/vnd.github+json"; "X-GitHub-Api-Version" = "2022-11-28" } -UseBasicParsing
                $selected = $selectedResponse.Content | ConvertFrom-Json
                if (-not [bool]$selected.github_owned_allowed) { throw "Organization Actions policy does not allow the GitHub-owned checkout action" }
            } elseif ([string]$actions.allowed_actions -ne "all") { throw "Organization Actions policy is unavailable" }
            $rulesetResponse = Invoke-WebRequest -Uri ($APIBase.TrimEnd('/') + "/orgs/" + [Uri]::EscapeDataString($boundOwner) + "/rulesets?includes_parents=true") -Headers @{ Authorization = "Bearer $token"; Accept = "application/vnd.github+json"; "X-GitHub-Api-Version" = "2022-11-28" } -UseBasicParsing
            foreach ($ruleset in @($rulesetResponse.Content | ConvertFrom-Json)) {
                if ([string]$ruleset.enforcement -ne "active") { continue }
                foreach ($rule in @($ruleset.rules)) {
                    if ([string]$rule.type -eq "merge_queue") { throw "Organization policy requires an unsupported merge queue" }
                    if ([string]$rule.type -eq "pull_request" -and [int]$rule.parameters.required_approving_review_count -gt 0) { throw "Organization policy requires human review" }
                }
            }
        } catch {
            throw "Unpublished organization repository policy cannot be proved before Platform mutation: $($_.Exception.Message)"
        }
    }
}
$hasher = [Security.Cryptography.SHA256]::Create()
try { $digest = ([BitConverter]::ToString($hasher.ComputeHash([Text.Encoding]::UTF8.GetBytes($token)))).Replace("-", "").ToLowerInvariant() } finally { $hasher.Dispose() }
[ordered]@{ login = [string]$identity.login; user_id = [long]$identity.id; owner = $boundOwner; owner_type = $ownerType; repository = $repositoryID; private = ($Visibility -eq "private"); publication_state = $PublicationState; scopes = $scopes; fingerprint_sha256 = $digest } | ConvertTo-Json -Depth 4
