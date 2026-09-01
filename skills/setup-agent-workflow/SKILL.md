---
name: setup-agent-workflow
description: Make the current repository usable by Agent Workflow through the Repository Setup Journey.
---

# Setup Agent Workflow

Treat the current directory as the setup target. Setup is complete only after
the Host can run the Worker, GitHub is usable, one Repository Watch exists,
and the native Control Plane records a successful GitHub Issues poll.

Do not use a Bundle, Setup Plan, PAT, repository contract, labels, remote
mutation, or a PATH installation. The user owns GitHub CLI login and Worker
authentication. Report either prerequisite precisely and let the user or the
owning capability resolve it before continuing.

## 1. Stateless preflight

Before downloading or installing anything, run the read-only preflight from
this installed skill. It requires the active `github.com` GitHub CLI login,
rejects detached HEAD, resolves the containing Git root when present, and
reports the intended GitHub repository and current Issues capability.

```powershell
$preflight = & "$skillRoot\scripts\test-repository-preflight.ps1" -RepositoryPath (Get-Location) | ConvertFrom-Json
```

Do not try to create a GitHub login, configure Git author identity, alter an
origin, repair divergent history, or make a repository mutation during this
step. An absent remote repository is not itself a blocker; creation permission
is proved only by the later create request.

## 2. Acquire the Host Executable

Workflow supports Windows x64 and macOS Apple Silicon. Resolve Workflow Home
to `%LOCALAPPDATA%\AgentWorkflow` on Windows or
`~/Library/Application Support/AgentWorkflow` on macOS. Reuse a working
stable executable at `WorkflowHome/bin/workflow(.exe)`; upgrades are explicit.
When it is missing or cannot start, select the latest immutable
`workflow-vX.Y.Z` release in the fixed repository policy, download exactly the
matching platform executable, verify the GitHub asset SHA-256 digest, and
atomically replace the stable file:

```powershell
$host = & "$skillRoot\scripts\install-workflow-host.ps1" -WorkflowHome $workflowHome | ConvertFrom-Json
$workflow = [string]$host.executable_path
```

The script never places Workflow on `PATH`. Run Setup only by this absolute
path. Until `workflow-v0.0.1` exists, report the release blackout rather than
falling back to a legacy asset.

## 3. Configure Worker execution authentication

Before direct Setup, obtain one explicit Worker Execution Authentication
selection. Never infer a mode from an API key, endpoint, model, or Codex
cache. On Windows, configure `codex_login` with the installed executable's
`execution-auth --mode codex_login`; if it reports not ready, ask the user to
complete `codex login` outside Setup and stop. For `api_key`, pass the key only
on standard input to `execution-auth --mode api_key --base-url <endpoint>
--model <model> --api-key-stdin`. It probes before persisting the selection, so
a failure leaves the previous selection unchanged. Never put an API key in an
argument, output, or repository data. On macOS, a verified Codex login is the
available direct-Setup authentication path.

## 4. Run direct Setup

Invoke one direct reconciliation command. It prepares the selected Worker
container plumbing, reads Worker authentication readiness, reconciles Git and
GitHub, creates or reuses the Watch, and registers the fixed native service
only when absent. It never stages files, changes remotes, merges, rebases,
resets, force-pushes, configures branch policy, or writes Workflow content to
the target repository.

```powershell
& $workflow setup --workflow-home $workflowHome
```

An API-key authentication configuration that becomes ready permits an
immediate rerun. A Codex-login prerequisite is an external interactive pause:
wait for the user to confirm login, rerun preflight, and invoke the same
absolute command again.

## 5. Completion and blockers

`Repository Ready` requires a Watch successful-poll timestamp after the new
Watch registration or native-service registration performed in this run. An
empty Issues response is sufficient. Do not create a test Issue or manually
start/reload the service.

For a history conflict, leave both histories unchanged and explain that the
user may separately reconcile Git history before rerunning Setup. For a
timed-out first poll, present only the repository, stage, latest runtime
diagnostic, native-service status, and one next action. Successful effects are
not rolled back; a rerun always reads actual state and resumes forward.
