# Production toolchain baseline

The final production decision follows
[production-qualification.md](production-qualification.md); Doctor is a hard
prerequisite, not the approval itself.

`config/toolchain.json` is the machine-readable production baseline checked by
`workflow doctor`. Every executable version and artifact is immutable:

- Codex CLI is pinned to an exact package version.
- GitHub CLI is installed from its official Linux amd64 release archive rather
  than Debian's older package, and is pinned by exact version and SHA-256.
  The npm client is used only to install Codex and is removed from the runtime
  image with its dependency tree before the Worker contract is checked.
- Go is pinned to an exact Linux amd64 archive version and official SHA-256
  checksum. Doctor verifies `go version` inside the exact Worker image.
- `no-mistakes` is pinned to a verified upstream commit plus a distinct fork
  commit, immutable fork release, and Linux release-asset checksum. Doctor
  proves the upstream commit is the fork commit's merge base, the release
  targets the fork commit, and the executable packaged in the exact
  manifest-pinned Worker image has one full Go `vcs.revision` equal to that
  fork commit and one `vcs.modified=false` setting. Its abbreviated
  human-readable version output and any repository-external bootstrap
  installation on the host are not Worker provenance. The
  [Worker Dockerfile](../../deploy/worker/Dockerfile) pins `procps` to an exact
  version from the immutable Debian snapshot because the supported container
  fallback for the `no-mistakes` daemon requires `ps`; candidate CI and Doctor
  both start the daemon and require its status check to succeed.
- The Worker source inputs name a version and GHCR repository. The exact
  registry digest is recorded only after an accepted main commit is published
  in its source-keyed `worker-release.json` GitHub Release asset.
- Every candidate and published Worker image produces an SPDX 2.3 SBOM and is
  scanned with Grype. A fixable High-or-greater finding fails the build. The
  immutable release contains exactly `worker-release.json` and
  `worker-sbom.spdx.json`; Doctor verifies the manifest-bound SBOM checksum and
  the successful publisher run before activating the image.
  A VEX statement cannot waive a fixable finding; any future VEX must name one
  vulnerability and package and include evidence that the affected code is not
  executable in the Worker contract.
- The dedicated GitHub integration repository and its required workflow path
  are explicit. The repository may be public or private, but its owner must
  match the configured Control Plane GitHub App owner. Branch protection is not a
  prerequisite: that owner retains sole merge authority. Its deployable
  workflow verifies only the live GitHub repository contract and does not
  require a copy of the Control Plane source tree. It can be manually rerun
  with `workflow_dispatch` after a visibility change. Control Plane tests and
  vetting remain in this repository's `worker-contract` CI, which runs for all
  Go source and module changes.
- Set the integration repository's `WORKFLOW_INTEGRATION_REPOSITORY` and
  `WORKFLOW_GATEWAY_CREDENTIAL_OWNER` Actions variables to those configured
  values. Its contract workflow fails closed unless the variables, runner
  repository, and canonical GitHub repository metadata all agree.
- The trusted host uses one GitHub App installed for all owner repositories with
  at least metadata/actions/checks read and contents/issues/pull-requests write.
  The same installation serves host-side observations and Gateway mutations.
  Its private key is the fixed PEM file configured by `private_key_file`; SQLite
  records only the App and installation IDs, PEM SHA-256 fingerprint, owner,
  integration repository, and successful live-contract time. Each process
  caches the installation token until shortly before GitHub's forced expiry.

Create and install one GitHub App for **All repositories**. Configure `Metadata: read`,
`Actions: read`, `Checks: read`, `Contents: write`, `Issues: write`, and
`Pull requests: write`, download its private key, and place it at
`C:\ProgramData\workflow\github-app.pem`.
This command
verifies Actions read, pushes a temporary Candidate commit, and calls that
commit's check-runs endpoint to verify Checks read before performing the
remaining idempotent writes and cleaning up its temporary branch, issue, and PR
in the dedicated integration repository. Only after the complete live contract,
including cleanup, passes does it record the discovered installation and resume
writes. During
verification, the durable Gateway
rotation pauses new writes and safely recovers an expired claim before the live
contract runs; a failed replacement leaves writes paused. A Gateway that starts
without its verified credential likewise persists the pause and projects one
recovery request to each affected repository Workflow Inbox:

```powershell
go run ./cmd/workflow credential provision `
  --config config/toolchain.json `
  --database C:\ProgramData\workflow\workflow.db `
  --app-id <GITHUB_APP_ID>
```

Run the complete target-host contract with:

```powershell
go run ./cmd/workflow doctor `
  --config config/toolchain.json `
  --workflow-repository skyhuang233/ai-engineer-workflow `
  --database C:\ProgramData\workflow\workflow.db `
  --codex-auth-file $env:USERPROFILE\.codex\auth.json `
  --report docs/operations/doctor-report.md
```

The Control Plane authenticates Ticket Agents with the trusted operator's
existing ChatGPT login cache. Run `codex login status` before starting the
workflow and complete `codex login` if necessary. `doctor`, `run-ticket`, and
`poll-github` default to `CODEX_HOME\auth.json`, or
`$env:USERPROFILE\.codex\auth.json` when `CODEX_HOME` is unset; use
`--codex-auth-file` to select another absolute cache path. The cache must use
ChatGPT authentication. It is atomically copied only when a Ticket Session has
no `auth.json`, so a Session-local cache refreshed by Codex is never
overwritten.

Ticket Agents are trusted with this cache. [ADR-0039](../adr/0039-seed-ticket-sessions-from-host-chatgpt-auth.md)
owns the credential threat model, redaction boundary, and terminal corruption
recovery contract.

[ADR-0004](../adr/0004-centralize-external-writes.md) owns the trusted Worker
container's Codex sandbox, Docker privilege, and GitHub credential boundary.
[ADR-0008](../adr/0008-persist-workspaces-per-ticket-session.md) owns Ticket
Workspace persistence and its repository-local LF policy.

Doctor performs a real create-and-resume request inside the pinned Worker
image using a temporary copy of this cache and supplies the exact Candidate
structured-output schema to both turns. The check fails unless Codex accepts
that schema, returns a valid Candidate response, and recalls the first turn's
nonce after resume. A version-only Codex check is not sufficient: missing or
rejected authentication fails the report before the Worker image is activated.
The temporary copy is removed after the probe, and credential contents are
never included in the report.

Doctor's production check runner has one fixed 10-minute shared deadline. The
budget includes a cold pull of the exact Worker digest and the real Codex
create-and-resume probe; reaching the deadline fails the run.

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
applies retry backoff, and acquires a fenced per-repository SQLite poll lease
before minting a Control Plane GitHub App installation token or making GitHub requests. Concurrent
pollers therefore cannot both pass the same `NextAttemptAt` boundary. It runs
the approved Plan Root control pass before
projecting the repository Workflow Inbox, so an eligible Delivery Plan is
active before the Gateway admits that projection. It then reconciles every
active ticket pull request and deduplicates newly observed reviews and comments
before the owning Ticket Session is resumed. `--review-feedback`,
`workflow answer-inbox`, and `workflow recover-inbox-delivery` are
privileged local Control Plane operations: run them only on the trusted Control
Plane host. `answer-inbox` forwards the resulting inbox projection through the
Gateway control-plane credential; that transport credential is not the local
operator authorization boundary. If an uncertain Workflow Inbox delivery
exhausts reconciliation, recover the rejected generation with the delivery key
shown in its Needs Attention prompt or Gateway recovery log. Run `workflow
recover-inbox-delivery --repository owner/repository` to list recoverable keys
with their stable Workflow Inbox question ids, including legacy rejected
deliveries, then run `workflow recover-inbox-delivery
--repository owner/repository --delivery delivery-key --question question-id
--answer retry` only after confirming
that the historical projection is absent or safe to resolve. The recovery
records that authorization, re-observes without replaying a superseded
projection, and atomically queues the current Needs Attention projection behind
it. They use the same durable queue for manual routing and decisions.
`workflow reconcile-delivered` checks merged pull requests
for reachability from `main` and freezes the plan in Needs Attention when a
pull request closes without merge.

On Docker Desktop, pass the Worker-facing Gateway address through
`--gateway-url` (for example `http://host.docker.internal:8787`). Use
`--gateway-control-url` for the host-side Plan and Workflow Inbox projections
(for example `http://127.0.0.1:8787`). When the latter is omitted, it defaults
to `--gateway-url` for backward compatibility.

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
and its checksum-bound SPDX SBOM must be the only two assets for that exact
source-keyed Worker release and must be published by the fixed
`publish-worker` push workflow after an unambiguous non-bot merge by the
configured owner. A successful complete run atomically makes that digest the
Active Worker Image for new Worker Runs; existing runs remain pinned to their
recorded image. [ADR-0009](../adr/0009-adopt-no-mistakes-as-delivery-controller.md)
owns Delivery Controller recovery runtime selection, Candidate provenance, and
Worker audit requirements. After a production activation, save the redacted
doctor report and record its result in Issue #7.

## Upgrade rule

Never edit only one version string. A toolchain upgrade is accepted only after:

1. recording the selected tool's exact upstream version and, for
   `no-mistakes`, its full verified upstream and fork commits and immutable
   fork release;
2. recording and verifying the official or release-asset SHA-256 for every
   downloaded archive;
3. updating the machine pins and immutable Worker build inputs together;
4. building and testing on the PR without publishing;
5. having the owner accept and merge the PR to main;
6. letting GitHub Actions publish the image and authoritative Release Manifest;
7. running unit, Docker, Codex Candidate-schema create/resume, SQLite, Gateway,
   and dedicated GitHub contract checks, which activates the verified digest
   for new Worker Runs.

Floating tags such as `latest`, floating branches such as `main`, and
unversioned local executables are not production inputs.
