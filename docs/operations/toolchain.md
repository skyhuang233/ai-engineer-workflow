# Production toolchain baseline

`config/toolchain.json` is the machine-readable production baseline checked by
`workflow doctor`. Every executable version and artifact is immutable:

- Codex CLI is pinned to an exact package version.
- `no-mistakes` is pinned to an upstream release, verified commit, fork
  repository, fork release, and Linux release-asset checksum. Doctor reads the
  installed executable's full immutable Go `vcs.revision` build metadata, so
  its abbreviated human-readable version output is not used as provenance.
- The Worker source inputs name a version and GHCR repository. The exact
  registry digest is recorded only after an accepted main commit is published
  in its source-keyed `worker-release.json` GitHub Release asset.
- The dedicated GitHub integration repository and its required workflow path
  are explicit. The repository may be public or private, but its owner must
  match the configured Gateway Credential owner. Branch protection is not a
  prerequisite: that owner retains sole merge authority. Its deployable
  workflow verifies only the live GitHub repository contract and does not
  require a copy of the Control Plane source tree. It can be manually rerun
  with `workflow_dispatch` after a visibility change. Control Plane tests
  remain in this repository's `worker-contract` CI, which runs for all Go
  source and module changes.
- Set the integration repository's `WORKFLOW_INTEGRATION_REPOSITORY` and
  `WORKFLOW_GATEWAY_CREDENTIAL_OWNER` Actions variables to those configured
  values. Its contract workflow fails closed unless the variables, runner
  repository, and canonical GitHub repository metadata all agree.
- The Gateway uses one fine-grained PAT for all owner repositories with exactly
  metadata/actions read and contents/issues/pull-requests write. The secret
  exists only in Windows Credential Manager and Control Plane memory. SQLite
  records only its SHA-256 fingerprint and successful live-contract evidence.
  GitHub does not expose an API that proves a fine-grained PAT has no additional
  permissions; selecting exactly this configuration is the owner's declaration,
  while the live contract machine-verifies every required positive capability.

Provision or rotate the Gateway Credential. This hidden-input command performs
real, idempotent writes in the dedicated integration repository and cleans up
its temporary branch, issue, and PR. During replacement, the durable Gateway
rotation pauses new writes and safely recovers an expired claim before the live
contract runs; a failed replacement leaves writes paused. A Gateway that starts
without its verified credential likewise persists the pause and projects one
recovery request to each affected repository Workflow Inbox:

```powershell
go run ./cmd/workflow credential provision `
  --config config/toolchain.json `
  --database C:\ProgramData\workflow\workflow.db
```

Run the complete target-host contract with:

```powershell
go run ./cmd/workflow doctor `
  --config config/toolchain.json `
  --workflow-repository skyhuang233/workflow `
  --database C:\ProgramData\workflow\workflow.db `
  --report docs/operations/doctor-report.md
```

`--workflow-repository` is the independently supplied repository that contains
the publisher workflow. Doctor requires it to exactly match
`worker.release_repository`, verifies that repository belongs to the configured
owner, and then uses only that repository for the release, source, workflow-run,
and manifest checks. Public and private repositories follow the same checks.

`workflow run-ticket` starts the pinned `no-mistakes` Delivery Controller in a
Docker Worker without GitHub credentials. The controller owns rebase, review,
tests, documentation checks, lint, Gateway-backed push and pull-request
updates, CI, and the review-driven revision cycle. `run-ticket` never
dispatches GitHub mutations itself; it requires the credential-isolated Gateway
URL and passes it only to the pinned controller. Run `workflow poll-github` as
the persistent control-plane process; it records durable polling cursors,
applies retry backoff, reconciles every active ticket pull request, and
deduplicates newly observed reviews and comments before the owning Ticket
Session is resumed. `--review-feedback` and `workflow answer-inbox` are
privileged local Control Plane operations: run them only on the trusted Control
Plane host. `answer-inbox` forwards the resulting inbox projection through the
Gateway control-plane credential; that transport credential is not the local
operator authorization boundary. They use the same durable queue for manual
routing and decisions. `workflow reconcile-delivered` checks merged pull requests
for reachability from `main` and freezes the plan in Needs Attention when a
pull request closes without merge.

The workflow adapter enables the fork's workflow mode only for those Delivery
Controller runs and fails closed before launch if either its Delivery Cycle or
Revision Round identity is missing. It uses the Ticket Session ID as the stable
correlation key for that Delivery Cycle, the accepted Candidate Revision's
Worker Run ID as the Revision Round key, and the delivery lease's Run ID as
the unique per-invocation correlation ID. Recovery retains the Delivery Cycle
and Revision Round keys but creates a new correlation ID for its new delivery
lease. Ordinary standalone `no-mistakes` usage receives none of this
workflow-specific configuration.

The command fails closed if any check fails. In particular, a locally built
image is not evidence of publication: the pinned digest must resolve from the
registry. The Release Manifest's exact digest must resolve from GHCR and pass
the Docker contract. Doctor may activate the latest owner-accepted manifest
even after unrelated `main` commits, but only while every pinned toolchain
input remains current and the deterministic build-input identity still matches
both its source commit and current `main`. That identity covers the
`deploy/worker` Git tree, the pinned `publish-worker` workflow blob, and the
Worker toolchain inputs consumed by the build. The Worker tree includes an
immutable Debian snapshot and exact direct APT package versions, which are also
recorded as image labels for build provenance. Release and image tags contain
the declared Worker version and this identity, allowing an input change to
produce a new immutable release without a manual version bump. The manifest
must be the sole asset for that exact source-keyed Worker release and must be published by the fixed
`publish-worker` push workflow after an unambiguous non-bot merge by the
configured owner. A successful complete run atomically makes that digest the
Active Worker Image for new Worker Runs; existing runs remain pinned to their
recorded image. After a production activation, save the redacted doctor report
and record its result in Issue #7.

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
