---
name: setup-agent-workflow
description: Prepare the current Windows machine and repository for Agent Workflow.
---

# Setup Agent Workflow

Treat the current directory as the target. Keep discovery read-only and use the bundled scripts instead of reproducing host checks by hand. Before discovery, run `codex doctor --json` and `codex login status`. Parse the redacted doctor JSON even when the command exits nonzero: unrelated terminal checks may fail and must not override valid required checks. Continue only when the machine-readable doctor report has schema 1, an `ok` `auth.credentials` check with `stored ChatGPT tokens=true`, `stored auth mode=chatgpt`, and an absolute `auth file` beneath the `CODEX_HOME` reported by the `ok` `config.load` check, and login status reports ChatGPT. The Workflow CLI performs the same checks and resolves the source automatically; never ask an ordinary user to locate or configure a private Codex file.

1. Create a task-temporary directory, then run host inspection once and read its saved JSON:

   ```powershell
   $setupTaskRoot = Join-Path ([IO.Path]::GetTempPath()) ("workflow-setup-" + [Guid]::NewGuid().ToString("N"))
   New-Item -ItemType Directory -Path $setupTaskRoot | Out-Null
   $hostFactsPath = Join-Path $setupTaskRoot "host-facts.json"
   $hostFactsJSON = & scripts/inspect-host.ps1 -Repository (Get-Location) | Out-String
   [IO.File]::WriteAllText($hostFactsPath, $hostFactsJSON, (New-Object Text.UTF8Encoding($false)))
   $hostFacts = Get-Content -LiteralPath $hostFactsPath -Raw | ConvertFrom-Json
   ```

   If the directory is not a Git repository, explain that Git is required and ask once whether to run `git init`. Stop if declined, then rerun inspection into the same file if accepted.
   Record the confirmed owner, repository name, visibility, publication state, and domain layout once; pass these same values to every subsequent plan command without asking again.
   Require the already-approved classic PAT scopes `repo,workflow` for a personal owner. Organization ownership remains supported in the design but must stop with `requires an approved organization scope contract` until that additional credential contract is explicitly approved; do not infer or request extra scopes. Capture the verifier's redacted JSON result and pass its `owner_type` and `fingerprint_sha256` unchanged to the bootstrap planner. When the persisted credential is already live-verified, reuse those two fields from `$hostFacts.github_credential`; never calculate a fingerprint from an unverified value.
2. Determine the complete GitHub repository intent before Platform planning and before any Platform mutation. Ask once for the owner, repository name, and private/public visibility, then reuse that decision throughout the setup loop without asking again. For an existing canonical GitHub `origin`, present its owner as the candidate together with its repository name and ask the user to confirm them; do not silently choose them. With no `origin`, explicitly ask whether the target belongs to the user's personal account or an organization and obtain that exact owner, repository name and private/public visibility. A non-GitHub `origin` blocks. When no verified credential exists, ask for the classic PAT and pipe it to `scripts/verify-github-pat.ps1 -Owner <owner> -RepositoryName <name> -Visibility <private|public> -PublicationState <published|unpublished>`. A personal owner must verify personal identity, exact `repo,workflow` scopes, repository access or absence, Actions checkout feasibility, and review/merge-queue policy read-only. An organization owner stops pending the approved organization scope contract. If any fact cannot be proved, stop before producing a Platform Bootstrap Plan. Never fall back to another owner after rejection.

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

   Resolution and Platform Plan generation are a contract-validated, forward-only dry run: they verify and preview the exact repair but do not rewrite either pin. Only the later exact-digest apply may repair durable state. The resolver verifies the packaged trust policy, pinned public key, immutable fixed GitHub Release assets, source commit, signature, release identity, and Platform Setup Contract before returning paths. A missing pinned key or any version/pin disagreement blocks.

   Produce the Platform Plan only from the resolver output:

   ```powershell
   $platformPlanPath = Join-Path $setupTaskRoot "platform-plan.json"
   & scripts/new-platform-bootstrap-plan.ps1 -ManifestPath $resolvedRelease.manifest_path -SignaturePath $resolvedRelease.signature_path -HostFactsPath $hostFactsPath -OutputPath $platformPlanPath -GitHubOwner <confirmed-owner> -GitHubOwnerType <personal|organization> -GitHubPATFingerprintSHA256 <verified-fingerprint>
   ```

   Pass `-AllowUpgrade` to the planner only for the same explicitly confirmed upgrade. If it returns `plan_required`, show its complete readable projection and ask for one approval of the displayed SHA-256 digest.
3. After approval, run the installer with `$resolvedRelease.manifest_path`, `$resolvedRelease.signature_path`, the approved digest, and `$platformPlanPath`. It re-verifies trust before downloading the platform archive. Allow UAC, Docker first launch, Codex login restoration, and PAT entry only when the corresponding approved plan declares them. Supply a PAT to `workflow setup apply` over standard input; keep it out of arguments and ordinary output. Remove `$resolvedRelease.temp_directory` and `$setupTaskRoot` only after setup finishes or fails; never treat their paths as durable state.
4. Use the installed CLI for the remaining deterministic loop:

   ```powershell
   workflow setup plan --repo (Get-Location).Path --repository-name <confirmed-name> --visibility <private|public> --publication-state <published|unpublished> --domain-layout <single-context|multi-context>
   workflow setup apply --plan <plan-path> --approved-digest <digest>
   workflow setup verify --repo (Get-Location).Path
   ```

   Present each complete Onboarding Plan once. An approval authorizes only its exact digest. If facts drift, present the replacement plan rather than expanding the old one.
5. Finish only when verification reports both `platform_ready` and `repository_admitted`. Otherwise report the exact blocker and whether the same digest can continue forward.

Preserve user-owned Git state. Never stage, commit, stash, reset, clean, switch branches, push existing commits, or rewrite `origin` during discovery. The CLI owns approved mutations and safe readback.
