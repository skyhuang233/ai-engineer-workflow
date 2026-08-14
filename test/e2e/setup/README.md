# Windows Workflow Setup qualification harness

This gated harness runs only when `WORKFLOW_SETUP_E2E=1`. It requires a clean
Windows qualification host, Docker Desktop, an existing Codex ChatGPT login, a
classic PAT in `WORKFLOW_SETUP_E2E_PAT`, a disposable GitHub owner, and a release
pipeline driver capable of invoking `$setup-agent-workflow` and approving the
declared plans.

The driver receives an isolated user profile, Codex home, Workflow Home, target
repository path, scenario name, and evidence path through environment variables.
It must write the final structured setup response to that evidence path. The
harness never passes the PAT on a command line and fails if the token appears in
captured evidence.

Run only against disposable repositories:

```powershell
$env:WORKFLOW_SETUP_E2E = "1"
$env:WORKFLOW_SETUP_E2E_PAT = "<classic PAT>"
pwsh ./test/e2e/setup/setup-e2e.ps1 `
  -GitHubOwner <disposable-owner> `
  -DriverScript <release-pipeline-codex-driver.ps1>
```

The release pipeline must run the harness from the exact produced GitHub Release,
not from a development checkout. Repository deletion and Docker cleanup run in a
`finally` block; any cleanup failure fails qualification.
