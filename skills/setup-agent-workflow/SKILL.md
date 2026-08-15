---
name: setup-agent-workflow
description: Prepare the current Windows machine and repository for Agent Workflow.
---

# Setup Agent Workflow

Treat the current directory as the target. Keep discovery read-only and use the bundled scripts instead of reproducing host checks by hand. Before discovery, require `WORKFLOW_CODEX_AUTH_FILE` to be an absolute existing file explicitly supplied by the invoking Codex integration, run `codex login status`, and continue only when it reports ChatGPT login. Codex exposes no supported command that returns its private credential path, so never infer one from `CODEX_HOME` or the user profile; fail closed and ask for the explicit integration source when it is absent.

1. Run `scripts/inspect-host.ps1 -Repository (Get-Location)` and read its JSON result. If the directory is not a Git repository, explain that Git is required and ask once whether to run `git init`. Stop if declined, then rerun inspection if accepted.
2. When no verified credential exists, ask for the classic PAT and pipe it to `scripts/verify-github-pat.ps1`; use the returned login as the default bound owner. Obtain the official stable Platform Release Manifest and its detached signature, or the exact version requested by the user. Run `scripts/new-platform-bootstrap-plan.ps1` with `-ManifestPath`, `-SignaturePath`, the host-facts file, and `-GitHubOwner <owner>`. The script must verify the packaged trust policy, pinned public key, release identity, and signed provenance before producing a plan. If it returns `plan_required`, show its complete readable projection and ask for one approval of the displayed SHA-256 digest.
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
