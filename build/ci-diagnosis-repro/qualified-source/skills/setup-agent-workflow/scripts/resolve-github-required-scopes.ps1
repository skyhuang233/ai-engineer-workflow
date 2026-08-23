[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [ValidateSet("personal", "organization")]
    [string]$OwnerType
)

$ErrorActionPreference = "Stop"
$personalScopes = @("repo", "workflow")

# This is the single enablement point for a future, explicitly approved
# organization credential contract. It intentionally stays empty today.
$approvedOrganizationScopes = @()
if ($OwnerType -eq "organization") {
    if ($approvedOrganizationScopes.Count -eq 0) {
        throw "Organization repository setup requires an approved organization scope contract"
    }
    @($personalScopes + $approvedOrganizationScopes)
    exit 0
}

$personalScopes
