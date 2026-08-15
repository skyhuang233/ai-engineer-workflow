# Agent Workflow

Agent Workflow turns a Windows machine into a single-host Control Plane and admits repositories through an exact, reviewable contract.

## Set up this machine and repository

Install the manually invoked Codex skill:

```powershell
npx skills@latest add skyhuang233/ai-engineer-workflow --skill setup-agent-workflow --agent codex -g -y
```

Open Codex in the repository directory and invoke:

```text
$setup-agent-workflow
```

The skill first checks whether the current directory is a Git repository. It then presents at most two complete approvals: a Platform Bootstrap Plan for host changes and an Onboarding Plan for repository changes. A classic GitHub PAT with `repo` and `workflow` scopes is stored in plaintext under the current user's Workflow Home; the PAT is available to trusted host components but never to Worker containers.

The first release supports a current-user Windows host, Docker Desktop, the invoking user's existing Codex ChatGPT login, and a GitHub `origin`. With no `origin`, setup can create a private GitHub repository. A non-GitHub `origin`, a different GitHub owner, or an organization that rejects classic PATs blocks setup.

The Control Plane runs only for the current session. Inspect it with `workflow status`, read its logs with `workflow logs`, stop it with `workflow stop`, and start it manually with `workflow serve`. Setup does not install a service, configure startup, recover after reboot, or uninstall the platform.

Repository Onboarding records the canonical repository, default branch, local source, Workspace roots, and Codex login source in Workflow Home. The installed Workflow Skill Bundle automatically binds a successfully published Delivery Plan Root to that record; no extra setup command or approval is required. `workflow serve` runs polling, reconciliation, Gateway, and scheduler loops only while the repository remains admitted and its runtime record is complete. Suspending one repository cancels only that repository's runtime.
