---
name: setup-agent-workflow
description: Set up the current Windows host from one verified Agent Workflow Bundle and onboard this repository.
---

# Setup Agent Workflow

Run only on Windows and treat the current directory as the first Repository
Onboarding target. Keep Git and Codex-login gates read-only. Ask for one
absolute local Workflow Home (default `%LOCALAPPDATA%\AgentWorkflow`) and one
classic GitHub PAT with `repo,workflow`; never put the PAT in command arguments
or ordinary output.

PAT verification must succeed before any GitHub mutation. Never call gh repo create or push the Onboarding target directly. Repository creation, baseline push, and contract Pull Request creation are owned exclusively by the exact approved `workflow onboarding apply` execution.

If the current unpublished directory is not yet a Git repository, ask whether
to initialize it. After acceptance run exactly `git init -b main`.
Never initialize an unpublished repository with the machine's implicit default branch;
the first Delivery Plan must use the supported `main` delivery base.

The download contract is one atomic private Workflow Release. Query only the
fixed repository in `trust/release-policy.json` with the Control Plane PAT.
Accept only a published, immutable, non-prerelease `workflow-vX.Y.Z` Release,
choose the highest semantic version, and require exactly these three uploaded
assets, each with positive size and `sha256:` digest metadata:
`workflow-windows-amd64.zip`, `workflow-release.json`, and
`worker-sbom.spdx.json`. Missing, extra, duplicate, or legacy component assets
fail closed; never fall back to `platform-v*` or `worker-v*`.

Download only `workflow-release.json` first and authenticate its bytes against
the GitHub asset digest. Before downloading either other asset, run the
PowerShell bootstrap verifier. Treat manifest `candidate_source_commit` and
`qualification_run_id` and `qualification_run_attempt` as qualified-candidate provenance. Separately require
the annotated Release tag to identify both an owner-created two-parent main
merge containing that candidate, require the authoritative qualification to
have completed before the merge, and require a successful `push` run of
`.github/workflows/publish-workflow.yml` for the merge commit. The verifier enforces schema 1 with
exact case-sensitive fields: unknown, duplicate, absent, mistyped, or invalid
values fail. It does not execute downloaded code. Then download the Bundle and
SBOM, verify their GitHub asset digests, and require those digests to equal the
manifest. Finally verify the Bundle before opening or executing it: require
root `platform-release.json`, matching Workflow version and Worker image,
protocol/schema versions, the complete listed regular-file inventory, and each
listed SHA-256. Reject unsafe, duplicate, absent, or unlisted archive entries.
Do not use a split checksum file, a Bootstrap Plan, an installed-CLI bridge, a
legacy resolver, or an arbitrary version selector.

Release-branch qualification has one explicit test-only acquisition boundary.
When the qualification harness sets `WORKFLOW_SETUP_QUALIFICATION=1`, require
absolute `WORKFLOW_SETUP_CANDIDATE_DIRECTORY`, exact
`WORKFLOW_SETUP_CANDIDATE_VERSION`, and full lowercase
`WORKFLOW_SETUP_CANDIDATE_SOURCE_COMMIT`, positive
`WORKFLOW_SETUP_CANDIDATE_QUALIFICATION_RUN_ID`, and positive
`WORKFLOW_SETUP_CANDIDATE_QUALIFICATION_RUN_ATTEMPT` values. Resolve that directory only
through `scripts/resolve-workflow-candidate.ps1`, then continue with the same
Bundle verification, consent, installation, readiness, and Repository
Onboarding steps below. Never enter this path during ordinary setup, never
fall back between candidate and published acquisition, and never describe a
candidate result as an installed published Release.

Use the repository-owned scripts in this order. `$downloadRoot` must be a new
empty temporary directory outside Workflow Home. The resolver authenticates
the manifest and publisher before acquiring the other two assets; the Bundle
verifier authenticates and inspects the archive before `$launcher` is assigned
or invoked:

Use pwsh for every Setup script and Launcher command except the native powershell.exe PAT verification shown below. Windows PowerShell 5.1 is not a supported host for the manifest and Bundle scripts. Never create a helper script inside the Onboarding target repository; if a temporary script is unavoidable, place it in a new temporary directory outside the repository and remove it before continuing.

```powershell
$patVerification = $pat | powershell.exe -NoProfile -NonInteractive -File `
  "$skillRoot\scripts\verify-github-pat.ps1" `
  -Owner $owner -RepositoryName $repositoryName `
  -Visibility $visibility -PublicationState $publicationState | ConvertFrom-Json
# verify-github-pat.ps1 reads [Console]::In.
# Do not pipe the PAT to verify-github-pat.ps1 through PowerShell's call operator (`| &`); invoke the native powershell.exe process exactly as above so it receives standard input.
if ($env:WORKFLOW_SETUP_QUALIFICATION -ceq '1') {
  $release = & "$skillRoot\scripts\resolve-workflow-candidate.ps1" `
    -CandidateDirectory $env:WORKFLOW_SETUP_CANDIDATE_DIRECTORY `
    -ExpectedVersion $env:WORKFLOW_SETUP_CANDIDATE_VERSION `
    -ExpectedSourceCommit $env:WORKFLOW_SETUP_CANDIDATE_SOURCE_COMMIT `
    -ExpectedQualificationRunID $env:WORKFLOW_SETUP_CANDIDATE_QUALIFICATION_RUN_ID `
    -ExpectedQualificationRunAttempt $env:WORKFLOW_SETUP_CANDIDATE_QUALIFICATION_RUN_ATTEMPT | ConvertFrom-Json
} else {
  $release = & "$skillRoot\scripts\resolve-workflow-release.ps1" -DownloadDirectory $downloadRoot | ConvertFrom-Json
}
$bundle = & "$skillRoot\scripts\verify-windows-bundle.ps1" `
  -BundlePath $release.bundle_path `
  -ExpectedSHA256 $release.bundle_sha256 `
  -ExpectedVersion $release.version `
  -ExpectedWorkerImage $release.worker_image | ConvertFrom-Json
$preparedBundle = & "$skillRoot\scripts\extract-verified-windows-bundle.ps1" `
  -BundlePath $release.bundle_path `
  -WorkingDirectory $downloadRoot `
  -VerifiedVersion $bundle.version `
  -VerifiedBundleDigest $bundle.bundle_digest `
  -ExpectedVersion $release.version `
  -ExpectedSHA256 $release.bundle_sha256 | ConvertFrom-Json
$version = [string]$preparedBundle.version
$bundleDigest = [string]$preparedBundle.bundle_digest
$verifiedExtractedBundle = [string]$preparedBundle.extracted_bundle
$verifiedReleaseManifest = @{
  manifest_path=[string]$release.manifest_path
  manifest_sha256=[string]$release.manifest_sha256
  source_commit=[string]$release.source_commit
}
$launcher = Join-Path $verifiedExtractedBundle 'setup\workflow-setup.exe'
```

Use `workflow-setup.exe` only through one UTF-8 JSON object on stdin and one
JSON result on stdout. Requests use schema 1. Ignore unknown response extension
fields, but stop on unknown statuses or malformed known evidence.

1. For ordinary onboarding, if the stable Dispatcher exists, call its active
   Launcher `verify`. `platform_ready` means reuse it with no Bundle download;
   `repair_required` reacquires the exact active Bundle. Missing or invalid
   state follows fresh-install path-conflict handling. Never silently adopt a
   nonempty unknown Workflow Home.
2. An explicit upgrade bypasses healthy reuse. Call Dispatcher `inspect` with
   `purpose=active_work_preflight` before target download; `active_work` asks
   the user to rerun later. Select latest eligible stable only when higher than
   the active version. Fresh chooses latest stable; repair and unfinished
   attempts choose their exact target.
3. For a verified target Bundle call `inspect` with `purpose=target_state`.
   Present its fixed concrete Platform Setup Consent capabilities. Send the
   accepted capability values in `apply`, or reuse only its returned unchanged
   `consent_id`. First setup may carry the PAT; later setup lets Launcher read
   its plaintext credential file. Do not create platform state before consent.
   The `persist_plaintext_pat` accepted capability is a JSON value with exactly
   `path` (the normalized absolute `WorkflowHome\state\credentials\github.pat`)
   and `owner` (the normalized GitHub owner collected before consent). Never
   use a delimiter-concatenated owner/path value.
4. After `apply: ready`, invoke the stable Dispatcher's active Launcher
   `verify`; continue only on `platform_ready`. A pre-activation
   `repair_required` retains the exact Consent and Attempt: reacquire no new
   target, call `inspect` for its reusable `consent_id`, then retry `apply`
   with that id (and the PAT again if persistence had not completed).
   `repair_required` is a forward repair of the same Bundle/Attempt, never
   automatic rollback.
5. Use the active versioned CLI only for Repository Onboarding. Generate,
   display, approve, apply, and verify the exact Onboarding Plan Digest for
   this current repository.
   Send the exact `$plan.onboarding_plan` as one JSON object on stdin using the
   command shape below; do not join, reconstruct, or
   wrap native command output.
   Do not reuse an earlier approved digest after any plan command failure:
   regenerate the complete Plan, display its current
   projection and digest, and obtain approval again.
   An `incomplete` result whose preceding effects are satisfied and whose only
   required effect is `repository-contract-pr` is the expected human merge
   gate, not a plan or command failure. Preserve and report that exact Plan
   Digest and Pull Request, then pause for the repository owner. Do not generate or apply another Onboarding Plan before the owner merges that exact Pull Request.
   After the owner confirms the merge, do not generate a new Plan or mutate the
   local branch directly. Resume the immutable stored Plan by calling
   `onboarding apply` with its preserved approved Digest and no stdin; the CLI
   reloads the exact canonical Plan from the active-generation Store. Then
   verify Repository Admission with that same Digest. Any other incomplete result
   is a blocker and must not be retried by guessing. Finish only at Platform
   Ready and Repository Admitted.

The Launcher owns generation state, migration, active-work fencing, Docker and
worker preparation, PATH reconciliation, and Control Plane lifecycle. The
skill only owns conversation, current-repository identity, bundle acquisition,
consent narration, and exact Onboarding Plan approval. Preserve user Git state.

Use these concrete command shapes; do not replace them with explanatory
pseudo-commands. `$dispatcher` is the stable `WorkflowHome\bin\workflow.exe`,
`$launcher` is the verified Bundle's `setup\workflow-setup.exe`, and every
setup object is UTF-8 JSON without extra stdin bytes:

```powershell
$verify = @{schema_version=1;operation='verify';workflow_home=$workflowHome} | ConvertTo-Json -Compress
$state = $verify | & $dispatcher setup verify | ConvertFrom-Json
# Explicit upgrade only:
$preflight = @{schema_version=1;operation='inspect';purpose='active_work_preflight';workflow_home=$workflowHome} | ConvertTo-Json -Compress
$preflight | & $dispatcher setup inspect | ConvertFrom-Json
$targetRequest = @{schema_version=1;operation='inspect';purpose='target_state';workflow_home=$workflowHome;target_version=$version;bundle_digest=$bundleDigest;github_owner=$owner;verified_release_manifest=$verifiedReleaseManifest}
$target = $targetRequest | ConvertTo-Json -Depth 16 -Compress
$inspection = $target | & $launcher | ConvertFrom-Json
# Do not invent or submit an empty consent surface. A fresh/changed target
# returns the exact field `required_capabilities`; present those values, then
# return that same nonempty array unchanged. An unchanged target instead
# returns the exact reusable consent_id and must not send capabilities.
if ($inspection.status -eq 'consent_required') {
  $acceptedCapabilities = @($inspection.evidence.required_capabilities)
  if ($acceptedCapabilities.Count -eq 0) { throw 'Launcher consent_required result omitted required_capabilities' }
  # Present exactly $acceptedCapabilities and collect the user's acceptance.
  $applyRequest = @{schema_version=1;operation='apply';workflow_home=$workflowHome;target_version=$version;bundle_digest=$bundleDigest;github_owner=$owner;accepted_capabilities=$acceptedCapabilities;pat=$pat}
} elseif ($inspection.status -eq 'ready') {
  $consentID = [string]$inspection.evidence.consent_id
  if ([string]::IsNullOrWhiteSpace($consentID)) { throw 'Launcher ready result omitted reusable consent_id' }
  $applyRequest = @{schema_version=1;operation='apply';workflow_home=$workflowHome;target_version=$version;bundle_digest=$bundleDigest;github_owner=$owner;consent_id=$consentID}
  if (-not [string]::IsNullOrWhiteSpace($pat)) { $applyRequest.pat = $pat }
} else {
  throw "Unexpected target_state status: $($inspection.status)"
}
$applyRequest.verified_release_manifest = $verifiedReleaseManifest
$apply = $applyRequest | ConvertTo-Json -Depth 16 -Compress
$apply | & $launcher | ConvertFrom-Json
# Repository authority is only the active versioned CLI, never the launcher:
$plan = & $dispatcher onboarding plan --workflow-home $workflowHome --repo $repo | ConvertFrom-Json
$plan.onboarding_plan | ConvertTo-Json -Depth 100 -Compress | & $dispatcher onboarding apply --workflow-home $workflowHome --repo $repo --onboarding-plan-digest $plan.onboarding_plan_digest
# Only after the owner confirms the exact Onboarding Pull Request was merged:
& $dispatcher onboarding apply --workflow-home $workflowHome --repo $repo --onboarding-plan-digest $plan.onboarding_plan_digest
& $dispatcher onboarding verify --workflow-home $workflowHome --repo $repo --onboarding-plan-digest $plan.onboarding_plan_digest
```

Treat `platform_ready`, `repair_required`, and `active_work` exactly as the
branching rules above prescribe. Reject any other status rather than guessing.
