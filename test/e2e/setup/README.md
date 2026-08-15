# Windows Workflow Setup qualification harness

This gated harness runs only when `WORKFLOW_SETUP_E2E=1`. It requires a clean
Windows qualification host, Docker Desktop, an existing Codex ChatGPT login, a
classic PAT in `WORKFLOW_SETUP_E2E_PAT`, a disposable GitHub owner, and a release
pipeline driver capable of invoking `$setup-agent-workflow` and approving the
declared plans. The repository-owned `codex-driver.ps1` is the reference
`DriverScript`: it installs an exact extracted skill bundle into the disposable
Codex home, runs `codex exec` with the strict result schema, approves only the
displayed setup-plan digests, and records disposable repositories for cleanup.

The driver receives an isolated user profile, Codex home, Workflow Home, target
repository path, scenario name, and evidence path through environment variables.
It must write the final structured setup response to that evidence path. The
harness never passes the PAT on a command line and fails if the token appears in
captured evidence.

Run only against disposable repositories:

```powershell
$env:WORKFLOW_SETUP_E2E = "1"
$env:WORKFLOW_SETUP_E2E_PAT = "<classic PAT>"
$env:WORKFLOW_SETUP_E2E_CLEANUP_TOKEN = "<separate classic PAT with delete_repo>"
pwsh ./test/e2e/setup/setup-e2e.ps1 `
  -GitHubOwner <disposable-owner> `
  -DriverScript ./test/e2e/setup/codex-driver.ps1 `
  -SkillSource <exact-extracted-release>/skills/setup-agent-workflow
```

`SkillSource` is an interface boundary, not a floating installer: the release
pipeline must point it at the skill directory extracted from the exact produced
GitHub Release, never a development checkout or an unpinned branch. The harness
copies the operator's existing Codex `auth.json` into the disposable profile;
the driver never captures or reports its contents. It also discovers newly
created `workflow-setup-e2e-*` repositories independently of the agent's final
response so cleanup remains possible after an interaction failure.

The driver output must satisfy `driver-result.schema.json`. Positive scenarios
must report both readiness gates; negative scenarios must report an exact
blocker. The cleanup token is removed from the environment before Codex starts;
only the harness uses it to delete disposable repositories in a `finally` block.
The Setup PAT needs only the product's `repo` and `workflow` scopes. Any
repository or Docker cleanup failure fails qualification.
