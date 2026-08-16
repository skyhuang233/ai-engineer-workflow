---
name: setup-agent-workflow
description: Prepare the current Windows machine and repository for Agent Workflow.
---

# Setup Agent Workflow

Treat the current directory as the target. Keep discovery read-only and use the bundled scripts instead of reproducing host checks by hand.

1. Confirm the absolute local Workflow Home once before the first host inspection. Use `%LOCALAPPDATA%\AgentWorkflow` only when the user did not select another absolute local path; reject relative and UNC paths. Keep this exact `$confirmedWorkflowHome` for every inspection, plan, apply, verify, and serve operation. Create a task-temporary directory, then run only the Git probe:

   ```powershell
   $confirmedWorkflowHome = [IO.Path]::GetFullPath(<confirmed-absolute-local-workflow-home>)
   $setupTaskRoot = Join-Path ([IO.Path]::GetTempPath()) ("workflow-setup-" + [Guid]::NewGuid().ToString("N"))
   New-Item -ItemType Directory -Path $setupTaskRoot | Out-Null
   $hostFactsPath = Join-Path $setupTaskRoot "host-facts.json"
   $gitFacts = & scripts/inspect-host.ps1 -Repository (Get-Location) -WorkflowHome $confirmedWorkflowHome -GitProbeOnly | ConvertFrom-Json
   ```

   Stop immediately if Git is unavailable. If the directory is not a Git repository, explain that Git is required and ask once whether to run `git init`. Stop immediately if declined. If accepted, run `git init`, then rerun the Git-only probe and require it to report `installed=true` and `is_repository=true`. Do not run full host inspection, Docker inspection, Control Plane inspection, release resolution, or `workflow.exe serve` until the Git-only probe confirms a repository. An unsafe or ambiguous repository probe is a rejection, not a reason to continue into Platform discovery.

   After the Git-only gate succeeds and before full host inspection, run `codex doctor --json` and `codex login status`. Parse the redacted doctor JSON even when the command exits nonzero: unrelated terminal checks may fail and must not override valid required checks. Continue only when the machine-readable doctor report has schema 1, an `ok` `auth.credentials` check with `stored ChatGPT tokens=true`, `stored auth mode=chatgpt`, and an absolute `auth file` beneath the `CODEX_HOME` reported by the `ok` `config.load` check, and login status reports ChatGPT. The Workflow CLI performs the same checks and resolves the source automatically; never ask an ordinary user to locate or configure a private Codex file.

   Only after that Git gate succeeds, run full host inspection once and read its saved JSON:

   ```powershell
   $hostFactsJSON = & scripts/inspect-host.ps1 -Repository (Get-Location) -WorkflowHome $confirmedWorkflowHome | Out-String
   [IO.File]::WriteAllText($hostFactsPath, $hostFactsJSON, (New-Object Text.UTF8Encoding($false)))
   $hostFacts = Get-Content -LiteralPath $hostFactsPath -Raw | ConvertFrom-Json
   ```

   Resolve both paths and require `$hostFacts.workflow_home` to equal `$confirmedWorkflowHome`; otherwise fail closed. Every later reinspection must pass `-WorkflowHome $confirmedWorkflowHome` again; never recover the default from process environment after this choice.

   Before release resolution, handle an already-authorized absent Control Plane process as an existing-authorization Control Plane restart. Use an explicit exact predicate; do not infer restart authority from a state label alone:

   ```powershell
   $sha256Pattern = '^[0-9a-f]{64}$'
   $durableAuthorityExact =
       $hostFacts.workflow.trust_state -eq "pinned" -and
       [bool]$hostFacts.workflow.owned -and
       [bool]$hostFacts.platform.installation_recorded -and
       -not [string]::IsNullOrWhiteSpace([string]$hostFacts.platform.version) -and
       [string]$hostFacts.workflow.version -ceq [string]$hostFacts.platform.version -and
       $hostFacts.platform.release_manifest_digest -cmatch $sha256Pattern -and
       $hostFacts.platform.platform_setup_contract_digest -cmatch $sha256Pattern -and
       $hostFacts.platform.workflow_cli_sha256 -cmatch $sha256Pattern -and
       [string]$hostFacts.workflow.sha256 -ceq [string]$hostFacts.platform.workflow_cli_sha256 -and
       -not [string]::IsNullOrWhiteSpace([string]$hostFacts.platform.release_bundled_files_json) -and
       $hostFacts.platform.release_bundled_files_digest -cmatch $sha256Pattern -and
       $hostFacts.platform.control_plane_plan_digest_sha256 -cmatch $sha256Pattern
   $staleNonLiveRecord =
       $hostFacts.control_plane.state -eq "stale" -and
       $null -ne $hostFacts.control_plane.runtime -and
       [string]$hostFacts.control_plane.runtime.platform_version -ceq [string]$hostFacts.platform.version -and
       [string]$hostFacts.control_plane.runtime.approved_platform_bootstrap_plan_digest_sha256 -ceq [string]$hostFacts.platform.control_plane_plan_digest_sha256
   $stoppedWithoutRecord =
       $hostFacts.control_plane.state -eq "stopped" -and
       $null -eq $hostFacts.control_plane.runtime
   $restartEligible = $durableAuthorityExact -and ($staleNonLiveRecord -or $stoppedWithoutRecord)
   ```

   Only when `$restartEligible` is true, run the installed `workflow.exe serve --workflow-home` command using the same confirmed home:

   ```powershell
   & (Join-Path $confirmedWorkflowHome "bin\workflow.exe") serve --workflow-home $confirmedWorkflowHome --approved-plan-digest $hostFacts.platform.control_plane_plan_digest_sha256
   ```

   Then rerun host inspection for `$confirmedWorkflowHome` and require `ready` runtime identity bound to that same version and digest. This restart reuses durable authorization and must not produce a new Platform Bootstrap Plan or ask for a new approval. Continue repository intent and credential discovery, but skip Platform release resolution, planning, and installation when the restarted Platform is ready.

   Incomplete or conflicting durable trust fails closed: never execute the installed Workflow CLI, guess a digest, or treat a Control Plane state alone as restart authority. Never restart `stopped` with a Runtime Record: that represents a live process whose health is unavailable. Never restart `ready` or `mismatched`. A `stale` record is eligible only because inspection proved its recorded process is not live and its exact runtime identity still matches the durable authorization. Continue only through the exact verified pin-repair path below; the restart shortcut remains unavailable until inspection proves the complete authorization identity and process absence.

   Record the confirmed owner, repository name, visibility, publication state, and domain layout once; pass these same values to every subsequent plan command without asking again.
   Require the already-approved classic PAT scopes `repo,workflow` for a personal owner. Organization ownership remains supported in the design but must stop with `requires an approved organization scope contract` until that additional credential contract is explicitly approved; do not infer or request extra scopes. Capture the verifier's redacted JSON result and pass its `owner_type` and `fingerprint_sha256` unchanged to the bootstrap planner. When the persisted credential is already live-verified, reuse those two fields from `$hostFacts.github_credential`; never calculate a fingerprint from an unverified value.
2. Determine the complete GitHub repository intent before Platform planning and before any Platform mutation. Ask once for the owner, repository name, and private/public visibility, then reuse that decision throughout the setup loop without asking again. For an existing canonical GitHub `origin`, present its owner as the candidate together with its repository name and ask the user to confirm them; do not silently choose them. With no `origin`, explicitly ask whether the target belongs to the user's personal account or an organization and obtain that exact owner, repository name and private/public visibility. A non-GitHub `origin` blocks. When no verified credential exists, ask for the classic PAT and pipe it to `scripts/verify-github-pat.ps1 -Owner <owner> -RepositoryName <name> -Visibility <private|public> -PublicationState <published|unpublished>`. A personal owner must verify personal identity, exact `repo,workflow` scopes, repository access or absence, and Actions checkout feasibility. Observe active review/merge-queue policy when GitHub makes those optional capabilities readable; a 403 from repository rulesets or branch protection is capability-unavailable, not a credential failure, so proceed in Owner-Guarded Mode. Positively observed review, merge-queue, or required-check policy remains applicable to the Onboarding Pull Request and must still be handled safely. An organization owner stops pending the approved organization scope contract. If any required fact cannot be proved, stop before producing a Platform Bootstrap Plan. Never fall back to another owner after rejection.

   Resolve the trusted Platform Release automatically. Do not accept release paths or URLs from the user or an unverified manifest. A fresh host may either use the default latest stable release or the user's explicitly confirmed exact version. `AllowUpgrade` is only for an installed lower version moving to a higher exact version:

   ```powershell
   $releaseArguments = @{ HostFactsPath = $hostFactsPath }
   # On a fresh host, add Version alone only when the user explicitly selected an exact version:
   $releaseArguments.Version = <confirmed-version>
   # Add AllowUpgrade only when an installed lower version is explicitly moving to that higher version:
   $releaseArguments.AllowUpgrade = $true
   $resolvedRelease = & scripts/resolve-platform-release.ps1 @releaseArguments | ConvertFrom-Json
   ```

   Branch on the inspected pin state without inventing release authority:

   - For a true fresh installation, omit `Version` only when the user selected latest stable.
   - When either verified durable pin is available, the verified backup pin automatically supplies exact repair authority if the primary is missing; omit `Version` for that exact pin repair. The resolver selects the pinned release and the later approved apply recreates the missing primary or backup.
   - When both verified pins are missing while the Workflow CLI exists, ask the user to confirm the exact installed version. Add `$releaseArguments.Version = <confirmed-exact-installed-version>` and do not add `AllowUpgrade`; never use latest-stable selection for a pinless existing installation.

   Resolution and Platform Plan generation are a contract-validated, forward-only dry run: they verify and preview the exact repair but do not rewrite either pin. Only the later exact-digest apply may repair durable state. The resolver accepts only the packaged canonical GitHub repository, an immutable stable release, its exact source commit and fixed manifest asset, then verifies every declared artifact checksum, release identity, provenance, and Platform Setup Contract before returning paths. Any repository, version, commit, pin, manifest, provenance, or checksum disagreement blocks.

   Produce the Platform Plan only from the resolver output:

   ```powershell
   $platformPlanPath = Join-Path $setupTaskRoot "platform-plan.json"
   & scripts/new-platform-bootstrap-plan.ps1 -ManifestPath $resolvedRelease.manifest_path -HostFactsPath $hostFactsPath -OutputPath $platformPlanPath -GitHubOwner <confirmed-owner> -GitHubOwnerType <personal|organization> -GitHubPATFingerprintSHA256 <verified-fingerprint>
   ```

   Pass `-AllowUpgrade` to the planner only for the same explicitly confirmed upgrade. If it returns `plan_required`, show its complete readable projection and ask for one approval of the displayed SHA-256 digest.
3. After approval, run the exact installer command:

   ```powershell
   & scripts/install-workflow-cli.ps1 -ManifestPath $resolvedRelease.manifest_path -PlanPath $platformPlanPath -ApprovedDigest <approved-digest>
   ```

   It re-verifies the manifest and approved plan before downloading the exact checksummed platform archive. Allow UAC, Docker first launch, Codex login restoration, and PAT entry only when the corresponding approved plan declares them. Supply a PAT to `workflow setup apply` over standard input; keep it out of arguments and ordinary output. Remove `$resolvedRelease.temp_directory` and `$setupTaskRoot` only after setup finishes or fails; never treat their paths as durable state.
4. Use the installed CLI for the remaining deterministic loop:

   ```powershell
   workflow setup plan --workflow-home $confirmedWorkflowHome --repo (Get-Location).Path --repository-name <confirmed-name> --visibility <private|public> --publication-state <published|unpublished> --domain-layout <single-context|multi-context>
   workflow setup apply --plan <plan-path> --approved-digest <digest>
   workflow setup verify --workflow-home $confirmedWorkflowHome --repo (Get-Location).Path
   ```

   Before approval or apply, parse the emitted Onboarding Plan and confirm its plan target.workflow_home exactly equals `$confirmedWorkflowHome`; `setup apply` intentionally has no Workflow Home override and consumes only that bound target. Present each complete Onboarding Plan once. An approval authorizes only its exact digest. If facts drift, present the replacement plan rather than expanding the old one.
5. Finish only when verification reports both `platform_ready` and `repository_admitted`. Otherwise report the exact blocker and whether the same digest can continue forward.

Preserve user-owned Git state. Never stage, commit, stash, reset, clean, switch branches, push existing commits, or rewrite `origin` during discovery. The CLI owns approved mutations and safe readback.
