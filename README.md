# Agent Workflow

Agent Workflow turns a Windows machine into a single-host Control Plane and admits repositories through an exact, reviewable contract.

## Set up this machine and repository

Install the manually invoked Codex skill:

```powershell
npx skills@latest add "skyhuang233/ai-engineer-workflow#workflow-v0.0.1" --skill setup-agent-workflow --agent codex -g -y
```

Open Codex in the repository directory and invoke:

```text
$setup-agent-workflow
```

The skill first checks whether the current directory is a Git repository. It then presents at most two complete approvals: Platform Setup Consent for host capabilities and an Onboarding Plan for repository changes. Accepted Platform Setup Consent can create the selected Workflow Home when it is absent; Repository Onboarding requires a ready active generation and never creates the home itself. A classic GitHub PAT with `repo` and `workflow` scopes is stored in plaintext under the current user's Workflow Home; the PAT is available to trusted host components but never to Worker containers.

Platform installation resolves the highest stable immutable `workflow-vX.Y.Z`
release from this canonical repository. A Workflow Release contains exactly
`workflow-windows-amd64.zip`, `workflow-release.json`, and
`worker-sbom.spdx.json`. Setup authenticates the manifest using GitHub asset
metadata, validates its complete schema before downloading the other assets,
then verifies the Bundle inventory and publisher provenance. Extra, missing, or
duplicate assets fail closed.

The Worker builds no-mistakes from its pinned repository commit with the pinned
Go toolchain. It also rebuilds the pinned GitHub CLI release commit with the
security-fixed `golang.org/x/mod` dependency and verifies the deterministic
binary hash recorded in the manifest. The Workflow Release—not a separate
no-mistakes Release—is the supply-chain boundary; Doctor verifies the final
assets, candidate qualification, owner merge and annotated provenance tag, publisher run and attempt,
and immutable Worker digest.

Platform and Worker are released atomically under one product version. Legacy
split-release artifacts and tags have been retired. Until the first owner-approved
`workflow-v0.0.1` is published, the repository is intentionally in a release
blackout: fresh installation and release-dependent repair or recovery are not
available, and consumers never fall back to legacy tags.

The Control Plane is supported and validated only on a current-user Windows host; Linux is limited to the Worker container runtime contract. The host requires Docker Desktop, the invoking user's existing Codex ChatGPT login, and a GitHub `origin`. Setup resolves that login automatically from the redacted machine-readable `codex doctor --json` report and confirms it with `codex login status`; ordinary users do not configure a credential path. With no `origin`, setup can create a private GitHub repository. A non-GitHub `origin`, a different GitHub owner, or an organization that rejects classic PATs blocks setup.

The Control Plane runs only for the current session. Inspect it with `workflow status`, read its logs with `workflow logs`, stop it with `workflow stop`, and start it manually with `workflow serve`. Setup does not install a service, configure startup, recover after reboot, or uninstall the platform.

Repository Onboarding records the canonical repository, default branch, and local source in Workflow Home, seeding its runtime record. The installed Workflow Skill Bundle automatically binds a newly created or activated Delivery Plan Root and completes that record before execution; no extra setup command or approval is required. `workflow serve` runs polling, reconciliation, Gateway, and scheduler loops only while the repository remains admitted and its runtime record is complete. Suspending one repository cancels only that repository's runtime.
