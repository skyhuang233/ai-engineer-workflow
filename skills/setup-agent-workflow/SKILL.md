---
name: setup-agent-workflow
description: Prepare the current Windows machine and repository for Agent Workflow.
---

# Setup Agent Workflow

Treat the current directory as the target. Keep discovery read-only and use the bundled scripts instead of reproducing host checks by hand. Before discovery, run `codex doctor --json` and `codex login status`. Parse the redacted doctor JSON even when the command exits nonzero: unrelated terminal checks may fail and must not override valid required checks. Continue only when the machine-readable doctor report has schema 1, an `ok` `auth.credentials` check with `stored ChatGPT tokens=true`, `stored auth mode=chatgpt`, and an absolute `auth file` beneath the `CODEX_HOME` reported by the `ok` `config.load` check, and login status reports ChatGPT. The Workflow CLI performs the same checks and resolves the source automatically; never ask an ordinary user to locate or configure a private Codex file.

1. Run `scripts/inspect-host.ps1 -Repository (Get-Location)` and read its JSON result. If the directory is not a Git repository, explain that Git is required and ask once whether to run `git init`. Stop if declined, then rerun inspection if accepted.
2. Determine the complete GitHub repository intent before Platform planning and before any Platform mutation. Ask once for the owner, repository name, and private/public visibility, then reuse that decision throughout the setup loop without asking again. For an existing canonical GitHub `origin`, present its owner as the candidate together with its repository name and ask the user to confirm them; do not silently choose them. With no `origin`, explicitly ask whether the target belongs to the user's personal account or an organization and obtain that exact owner, repository name and private/public visibility. A non-GitHub `origin` blocks. When no verified credential exists, ask for the classic PAT and pipe it to `scripts/verify-github-pat.ps1 -Owner <owner> -RepositoryName <name> -Visibility <private|public> -PublicationState <published|unpublished>`; personal identity or active organization-owner access, `repo`/`workflow` scopes, repository access or absence, organization administration/create policy, Actions checkout feasibility, review/merge-queue policy, and organization SSO must all verify read-only. If any fact cannot be proved, stop before producing a Platform Bootstrap Plan. Never fall back to another owner after rejection. If a Platform Installation exists, obtain the exact release identified by its durable version and manifest digest rather than querying latest. Use the official latest stable release only for the first installation; use a different release only when the user explicitly requested an upgrade, and then pass `-AllowUpgrade`. Run `scripts/new-platform-bootstrap-plan.ps1` with `-ManifestPath`, `-SignaturePath`, the host-facts file, and `-GitHubOwner <owner>`. The script must verify the packaged trust policy, pinned public key, release identity, and signed provenance before producing a plan. If it returns `plan_required`, show its complete readable projection and ask for one approval of the displayed SHA-256 digest.
3. After approval, run `scripts/install-workflow-cli.ps1` with the same release manifest and signature, approved digest, and generated plan. It re-verifies trust before downloading the platform archive. Allow UAC, Docker first launch, Codex login restoration, and PAT entry only when the corresponding approved plan declares them. Supply a PAT to `workflow setup apply` over standard input; keep it out of arguments and ordinary output.
4. Use the installed CLI for the remaining deterministic loop:

   ```powershell
   workflow setup plan --repo (Get-Location).Path
   workflow setup apply --plan <plan-path> --approved-digest <digest>
   workflow setup verify --repo (Get-Location).Path
   ```

   Present each complete Onboarding Plan once. An approval authorizes only its exact digest. If facts drift, present the replacement plan rather than expanding the old one.
5. Finish only when verification reports both `platform_ready` and `repository_admitted`. Otherwise report the exact blocker and whether the same digest can continue forward.

Preserve user-owned Git state. Never stage, commit, stash, reset, clean, switch branches, push existing commits, or rewrite `origin` during discovery. The CLI owns approved mutations and safe readback.
