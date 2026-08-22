# Windows Workflow Setup qualification harness

This gated harness runs only when `WORKFLOW_SETUP_E2E=1`. It requires a clean
Windows qualification host, Docker Desktop, an existing Codex ChatGPT login, a
classic PAT in `WORKFLOW_SETUP_E2E_PAT`, a disposable GitHub owner, and a release
pipeline driver capable of invoking `$setup-agent-workflow` and approving the
declared plans. The repository-owned `codex-driver.ps1` is the reference
`DriverScript`: it runs the README `npx skills@latest add` command against an
exact `#workflow-v...` release tag, runs `codex exec` with the strict result schema, approves only the
displayed setup-plan digests, and records disposable repositories for cleanup.

Release qualification takes its disposable owner, different-owner fixture,
Setup PAT, and cleanup token from `WORKFLOW_SETUP_E2E_GITHUB_OWNER`,
`WORKFLOW_SETUP_E2E_DIFFERENT_OWNER_REPOSITORY`, `WORKFLOW_SETUP_E2E_PAT`, and
`WORKFLOW_SETUP_E2E_CLEANUP_TOKEN` in the owner-started ephemeral runner's
process environment. The workflow does not replace those inputs with repository
variables or secrets.

The driver receives an isolated user profile, Codex home, Workflow Home, target
repository path, scenario name, and evidence path through environment variables.
It must write the final structured setup response to that evidence path. The
harness never passes the PAT on a command line and fails if the token appears in
captured evidence.

Run only against disposable repositories:

```powershell
$env:WORKFLOW_SETUP_E2E = "1"
$env:WORKFLOW_SETUP_E2E_PAT = "<classic PAT>"
$env:WORKFLOW_SETUP_E2E_CLEANUP_TOKEN = "<separate cleanup credential with list/delete capability>"
pwsh ./test/e2e/setup/setup-e2e.ps1 `
  -GitHubOwner <disposable-owner> `
  -DriverScript ./test/e2e/setup/codex-driver.ps1 `
  -EntrySkillSpec "skyhuang233/ai-engineer-workflow#workflow-vX.Y.Z" `
  -PlatformVersion X.Y.Z `
  -DifferentOwnerRepository <public-owner/public-repository>
```

`EntrySkillSpec` is an interface boundary, not a floating source: the release
pipeline must pin it to the exact produced Workflow Release tag with the skills
CLI `#workflow-v...` Git ref syntax, never an unpinned branch. Release-branch
qualification instead passes the local qualification checkout and proves its
`git rev-parse HEAD` equals `WORKFLOW_SETUP_CANDIDATE_SOURCE_COMMIT`. The harness resolves the
operator's source from the redacted machine-readable `codex doctor --json`
report, verifies its ChatGPT mode and `CODEX_HOME` boundary, and copies it into
the disposable profile; the driver never captures or reports its contents. The
test-only `WORKFLOW_CODEX_AUTH_FILE` override is not needed. It also discovers newly
created `workflow-setup-e2e-*` repositories independently of the agent's final
response so cleanup remains possible after an interaction failure.

The driver output must satisfy `driver-result.schema.json`. Positive scenarios
pause first at the exact Repository Onboarding pull request and, for the clean
new-repository scenario, again at the exact Worker pull request. The harness
verifies each approved digest, head revision, and required-check result, then
waits for the repository owner to authorize and perform that exact merge before
resuming the same Workflow Home. Qualification independently reads the exact
Plan's GitHub Control Plane projection and succeeds only after Repository
Admission, Ticket Delivered, and Plan Completed. Negative scenarios must report
an exact blocker. The cleanup credential is removed from the environment before
Codex starts; only the harness uses it to list and delete newly created
disposable repositories in a `finally` block.
The Setup PAT needs only the product's `repo` and `workflow` scopes. Any
repository or Docker cleanup failure fails qualification.

The standard mode clones `DifferentOwnerRepository` to create a real owner
mismatch. Organization classic-PAT policy is qualified separately so owner
mismatch cannot mask it:

```powershell
pwsh ./test/e2e/setup/setup-e2e.ps1 `
  -GitHubOwner <organization-that-rejects-classic-pats> `
  -DriverScript ./test/e2e/setup/codex-driver.ps1 `
  -EntrySkillSpec "skyhuang233/ai-engineer-workflow#workflow-vX.Y.Z" `
  -PlatformVersion X.Y.Z `
  -QualificationMode organization-policy `
  -ClassicPATRejectedRepository <organization/public-fixture>
```
