# Production qualification drill

This runbook is the release-acceptance procedure for a Self-Hosting Cutover. A
closed implementation issue, a green unit suite, or a published Worker image is
not production approval. Run every phase on the target Windows host against the
dedicated private Owner-Guarded integration repository, retain the named evidence, and
have the repository owner record the final decision in the qualification issue.

The release is not qualified unless the README's `npx skills@latest add ...`
command, followed by explicit `$setup-agent-workflow` invocation in a clean
Windows profile, reaches both gates:

- **Platform Ready:** signed Platform Release and exact CLI/bundle installation,
  Docker Linux `amd64` container/mount/network probe, owner-bound PAT scope and
  capability verification, live `workflow serve` health, and real Codex Worker
  create-and-resume all pass.
- **Repository Admitted:** the digest-bound Onboarding Pull Request is merged,
  every managed file/block/label/setting/check reads back, and the exact manifest
  digest is recorded as eligible for that repository.

Every temporary Docker container/image, Codex state copy, onboarding branch,
download, clone, and GitHub resource must be removed. Cleanup failure is a failed
qualification, even when the functional probe passed.

## Evidence directory

Create a new directory outside the repository for each attempt. Record its
absolute path in the qualification issue. It must contain the redacted Doctor
report, command transcripts, backup metadata and drill output, GitHub object
links, Worker Release Manifest and SBOM checksums, fault-injection results, and
the final approval or rejection. Never copy credentials, Codex state, prompts,
or complete diffs into it.

## 1. Admit the host and supply chain

1. Confirm Docker Desktop uses a Linux `amd64` engine and `codex login status`
   reports the configured ChatGPT identity.
2. Read the target repository metadata and fail the attempt unless its
   canonical owner matches the verified Control Plane GitHub Credential owner and
   `private` is `true`.
3. Supply the classic PAT through the trusted Codex setup task and confirm that
   it is stored only at `state\credentials\github.pat` beneath Workflow Home.
   Retain redacted evidence for login, owner binding, fingerprint, `repo` and
   `workflow` scopes, SSO/organization policy, and live capabilities. Never
   retain the PAT body.
4. Run `workflow setup verify` and `workflow doctor` exactly as documented in
   [toolchain.md](toolchain.md), using the production SQLite path and an evidence
   report path. Platform Ready and Repository Admitted must both pass.
5. Download the two assets from the accepted source-keyed Worker Release. The
   `worker-sbom.spdx.json` SHA-256 must equal `sbom_sha256` in
   `worker-release.json`; the manifest must record the exact Worker digest,
   GitHub CLI version/checksum, no-mistakes immutable fork release/checksum,
   successful publisher run, and the
   fail-closed Grype `high`/`only_fixed` policy.
6. Link the successful `worker-contract`, `publish-worker`, and dedicated
   `workflow-contract` runs. A warning, skipped gate, mutable tag, extra release
   asset, or locally built substitute is a rejection.

## 2. Exercise one complete Delivery Plan

Create a fresh Plan Root in the dedicated repository with two initially ready
Executable Tickets that change the same integration surface and one downstream
ticket blocked by both. Record all issue IDs, dependency links, branches, pull
requests, commits, Delivery Cycle/Revision Round IDs, Ticket Session IDs and Run
Lease generations.

Start the credential-isolated Gateway and one persistent `poll-github` process
with `--max-parallel-runs 2`. Activation must occur only after the complete DAG
has been reread and accepted. Before activation, capture evidence that the ready
frontier and Worker Run count are zero. After activation, capture two distinct
concurrent Ticket Sessions and Worker Runs. Each initial Ticket Agent must
implement the complete specification captured in its activated plan version
without retrieving the ticket from GitHub. Inspect each new Ticket Workspace's
repository-local Git configuration and require `workflow-ticket-agent` /
`workflow-ticket-agent@users.noreply.github.com`; require every implementation
Candidate to report the lowercase 40-character SHA that exactly equals its
Workspace `HEAD`; `null` is reserved for a Plan Amendment with no implementation
commit.

The owner then performs these interventions in order:

1. Submit actionable pull-request feedback. The same Ticket Agent, workspace,
   branch, pull request and Delivery Cycle must produce the next Revision Round.
2. Force each quality-gate outcome (`auto-fix`, `no-op`, `ask-user`, `skip`).
   Only the first two may continue automatically. Answer the human gates by
   stable Workflow Inbox question ID and retain the before/after projection.
3. Merge the first overlapping pull request. The second candidate must lose
   Merge-Ready, rebase onto the new `main`, rerun the complete no-mistakes chain,
   and update the same pull request before the owner merges it.
4. Confirm both merge revisions are reachable from `main`. Only then may the
   tickets become Delivered and the downstream ticket become claimable.
5. Let the downstream ticket complete through Gateway publication, human
   review and owner merge. No Worker may receive a GitHub write credential and
   no Gateway operation may merge or write `main` directly.

## 3. Recovery and human-control drill

Use fresh fixture plans where a terminal decision would prevent later steps.

- Pause new work by stopping the persistent poller after recording current
  leases; restart it with the identical database path, Plan Root number,
  Ticket Workspace root and Codex state root. Existing Ticket
  Sessions, workspaces, cursors and accepted revisions must resume without a
  duplicate Run or GitHub object.
- Stop an old Worker after its lease is visible, let a replacement generation
  take ownership, then resume the old Worker. Its push, PR and reply attempts
  must be rejected by both the SQLite and Gateway fences.
- Exercise [ADR-0009](../adr/0009-adopt-no-mistakes-as-delivery-controller.md)'s
  historical-Worker recovery contract. Preserve an accepted Candidate from a
  Worker Image that cannot start the Delivery Controller, activate a corrected
  release only after Doctor passes, and retry delivery. The retry must use the
  Active Worker Image without changing the accepted commit or its original
  runtime provenance. Retain both Worker audits and verify each launch's image,
  tools, mounts, Gateway host mapping, and absence of GitHub write credentials.
- Kill and restart the Control Plane around each outbox boundary: before the
  remote call, after an unknown remote result, and after remote confirmation.
  Reconciliation must find the same branch, PR, comment or projection rather
  than create another object.
- Submit and approve a Plan Amendment that affects one subgraph. The affected
  tickets must pause while an unrelated frontier remains dispatchable; a
  repeated answer must not create another plan version.
- Close an unmerged pull request. Verify the complete impact question, choose
  replacement in one fixture, and choose cancellation in another. Cancellation
  must revoke active leases and stop new dispatch without reverting delivered
  commits; repeat the decision to prove idempotency.
- Create an online SQLite backup, run `drill-backup`, restore it to an isolated
  path, and reconcile dry-run before replacing any production database. Verify
  sessions, leases, outbox, cursors and Inbox questions converge without remote
  duplication.
- Replace or revoke the classic PAT, then rerun the approved Platform Bootstrap
  repair. Writes and new scheduling must stay paused until owner, scopes, policy,
  and repository admissions pass again; no Worker state may contain the PAT or
  its credential path.

## 4. Automated negative evidence

Set `WORKFLOW_QUALIFICATION_DATABASE` to the absolute production SQLite path
whose Active Worker Image was activated by the successful Doctor run. Then run
`go test ./...`, `go vet ./...`, and the repository's race-enabled CI. The
Windows Delivery Source contract fails if that database, Docker, or the Linux
container engine is unavailable, and it runs the exact digest recorded in the
active Worker Release rather than a mutable tag. Run the fault-injection and
contract suites repeatedly, not only once. Use this baseline mapping and add
the successful run URL beside every row in the issue:

| Required invariant / boundary | Negative evidence |
| --- | --- |
| Activation is the final authority; Plan Root is not executable | `TestPollRunsControlPassBeforeWorkflowInboxProjection`, `TestActivationPathPersistsOneVersionAcrossRestart`, `TestDeliveredLabelDoesNotAuthorizeDelivery` |
| Blockers must be Delivered and capacity must exist | `TestReadyFrontierIsStableAndHonorsDeliveredBlockersAndCapacity`, `TestReadyFrontierReturnsNoTicketWhenCapacityIsFull` |
| One current Session/Agent/branch/PR/live Lease | `TestClaimReadyCreatesSessionRunAndLeaseAtomically`, `TestBootstrapRecoveryClaimHasOneWinner` |
| Lease generation and expected remote head fence writes | `TestGatewayRejectsZombieCommandAfterLeaseReplacement`, `TestGatewayRejectsRemoteHeadDriftBeforeExternalWrite`, `TestLeaseTakeoverCannotCommitAcrossInflightExternalWrite` |
| Outbox before call, unknown result reconciliation, no duplicate object | `TestGatewayUsesDurableOutboxAndReconcilesAnUncertainWrite`, `TestGatewayTreatsPostDeadlineSuccessAsUncertain`, `TestOutboxProcessingLeaseCanBeReclaimedAfterRestart` |
| Worker cannot choose target or receive GitHub write credentials | `TestSpecRejectsGitHubWriteCredentialsAndRequiresAuditInputs`, `TestGatewayDerivesRepositoryFromLeasedTicket`, `TestWorkspacePusherRejectsPushURLWithEmbeddedCredential` |
| Initial Worker receives the immutable activated ticket specification without GitHub access | `TestTicketBodyReturnsImmutableActivatedSpecification`, `TestResolveWorkerPromptUsesImmutableBodyForInitialRun`, `TestImplementationPromptCarriesPersistedTicketContract` |
| A live review claim resumes its original persisted prompt instead of the initial ticket body | `TestResolveWorkerPromptPreservesReviewRevisionExactly`, `TestAcquireTicketClaimRestoresClaimedReviewPromptBeforeControllerRun` |
| New and recovered workspaces use the non-owner ticket-agent Git identity | `TestControllerCreatesLFOnlyTicketWorkspaceDespiteHostAutoCRLF`, `TestControllerNormalizesExistingCRLFTicketWorkspaceDuringRecovery` |
| Implementation Candidate names its exact lowercase full workspace HEAD | `TestSchemaMeetsOpenAIStrictObjectRequirements`, `TestValidateStrictCandidateOutput`, `TestControllerRejectsImplementationCandidateWithNullCommit` |
| Workspace/Codex state outlive containers; replacement waits for expired-run cleanup | `TestControllerSnapshotsAndRestoresAnAbnormalWorkerRun`, `TestControllerBlocksReplacementUntilAgentRunFinishes` |
| Delivery recovery preserves legacy and current Candidates across runtime changes | `TestControllerRetryDeliveryPreservesLegacyCandidateWithoutSourceDigest`, `TestControllerRetriesFailedDeliveryAtAcceptedCandidateBoundaryWithActiveWorker` |
| Feedback is owner-classified, deduplicated and batched once | `TestActionablePullRequestFeedbackIncludesOwnerEventsOnly`, `TestReviewFeedbackDeduplicatesAndBatchesOneRevision` |
| Agent cannot merge, resolve threads or write `main` | `TestGatewayRejectsAgentPhasePublicationBeforeDeliveryController`, `TestGatewayAllowsDeliveryControllerCommandFromAcceptedCandidate` plus the real Gateway capability drill |
| Delivered requires owner merge and main reachability | `TestPullRequestReachedMainRejectsNonOwnerAndBotMergers`, `TestReconcileTicketPersistsMergeRevisionAndUnlocksDependentFrontier` |
| Active DAG changes only through approved Plan Amendment | `TestPlanAmendmentPausesOnlyAffectedSubgraphAndAppliesOneApprovedVersion`, `TestGatewayRejectsUnstructuredPlanBodyReplacement` |
| Workflow Inbox question fingerprints and answers are idempotent | `TestGatewayCredentialPauseUsesOneDurableInboxItemAndResumes`, `TestAnswerWorkflowQuestionQueuesInboxProjectionAtomically` |
| `ask-user`/`skip` cannot be auto-approved | `TestParseDeliveryOutcomeEnforcesQualityGateActions`, `TestControllerPausesHumanQualityGateAndRetriesItsExactAnswer` |
| Global capacity/host pressure pauses dispatch without killing active Runs | `TestHostPressurePausesNewDispatchWithoutChangingActiveRuns` |

The live fixture must additionally repeat every remote mutation boundary and
record the stable GitHub object IDs before and after restart. A flaky, skipped,
time-dependent, or non-repeatable test is a failed qualification result.

## 5. Owner sign-off

Add one comment to the qualification issue containing:

- decision: `APPROVED` or `REJECTED`;
- owner login and UTC timestamp;
- target host, Docker, Codex, Go and workflow source versions;
- exact Worker digest, Worker Release URL, manifest SHA-256 and SBOM SHA-256;
- Doctor report checksum and links to contract/E2E/fault-injection evidence;
- Plan Root, ticket, PR, Workflow Inbox and backup-drill evidence links;
- every accepted exception, or the literal statement `exceptions: none`.

Only an explicit `APPROVED` comment from the configured non-bot owner permits
Self-Hosting Cutover. Automation must never infer approval or close the
qualification issue from passing checks.
