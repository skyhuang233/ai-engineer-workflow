# Production toolchain baseline

`config/toolchain.json` is the machine-readable production baseline checked by
`workflow doctor`. Every executable version and artifact is immutable:

- Codex CLI is pinned to an exact package version.
- `no-mistakes` is pinned to an upstream release, verified commit, fork
  repository, fork release, and Linux release-asset checksum.
- The Worker source inputs name a version and GHCR repository. The exact
  registry digest is recorded only after an accepted main commit is published
  in its `worker-release.json` GitHub Release asset.
- The dedicated GitHub integration repository and its required workflow are
  explicit and public. Branch protection is not a prerequisite: the repository
  owner retains merge authority.
- The Gateway uses one fine-grained PAT for all owner repositories with exactly
  metadata/actions read and contents/issues/pull-requests write. The secret
  exists only in Windows Credential Manager and Control Plane memory. SQLite
  records only its SHA-256 fingerprint and successful live-contract evidence.
  GitHub does not expose an API that proves a fine-grained PAT has no additional
  permissions; selecting exactly this configuration is the owner's declaration,
  while the live contract machine-verifies every required positive capability.

Provision or rotate the Gateway Credential. This hidden-input command performs
real, idempotent writes in the dedicated integration repository and cleans up
its temporary branch, issue, and PR:

```powershell
go run ./cmd/workflow credential provision `
  --config config/toolchain.json `
  --database C:\ProgramData\workflow\workflow.db
```

Run the complete target-host contract with:

```powershell
go run ./cmd/workflow doctor `
  --config config/toolchain.json `
  --database C:\ProgramData\workflow\workflow.db `
  --report docs/operations/doctor-report.md
```

`workflow run-ticket` starts the pinned `no-mistakes` Delivery Controller in a
Docker Worker without GitHub credentials. The controller owns rebase, review,
tests, documentation checks, lint, Gateway-backed push and pull-request
updates, CI, and the review-driven revision cycle. `run-ticket` never
dispatches GitHub mutations itself; it requires the credential-isolated Gateway
URL and passes it only to the pinned controller. Run `workflow poll-github` as
the persistent control-plane process; it records durable polling cursors,
applies retry backoff, reconciles every active ticket pull request, and
deduplicates newly observed reviews and comments before the owning Ticket
Session is resumed. `--review-feedback` uses that same durable queue for manual
routing. `workflow reconcile-delivered` checks merged pull requests
for reachability from `main` and freezes the plan in Needs Attention when a
pull request closes without merge.

The command fails closed if any check fails. In particular, a locally built
image is not evidence of publication: the pinned digest must resolve from the
registry. The Release Manifest's exact digest must resolve from GHCR and pass
the Docker contract. A successful complete run atomically makes that digest the
Active Worker Image for new Worker Runs; existing runs remain pinned to their
recorded image. The report must be reviewed before the production baseline is
approved.

## Upgrade rule

Never edit only one version string. A toolchain upgrade is accepted only after:

1. recording the new upstream release and full verified commit;
2. publishing a new immutable fork release;
3. verifying release-asset checksums before use;
4. building and testing on the PR without publishing;
5. having the owner accept and merge the PR to main;
6. letting GitHub Actions publish the image and authoritative Release Manifest;
7. running unit, Docker, Codex resume, SQLite, Gateway, and dedicated GitHub
   contract checks, which activates the verified digest for new Worker Runs.

Floating tags such as `latest`, floating branches such as `main`, and
unversioned local executables are not production inputs.
