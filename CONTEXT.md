# Agent Workflow

This context describes the language used to plan, coordinate, and deliver software changes through autonomous agents.

## Language

**Delivery Plan（交付计划）**:
An approved intended outcome and the complete set of Executable Tickets that collectively deliver it.
_Avoid_: Executable Ticket, dependency chain, pull request

**Plan Root（计划根节点）**:
The non-executable record that owns a Delivery Plan and defines its authoritative membership boundary.
_Avoid_: Executable Ticket, ready-for-agent issue, epic without membership

**Workflow Inbox（人工收件箱）**:
The single queue where every unresolved human decision from agents, quality gates, plan changes, cancellation, or recovery is presented and answered by stable question id.
_Avoid_: Plan status dashboard, pull-request review, duplicated question thread

**Executable Ticket（可执行票据）**:
A work item scoped to produce one independently reviewable delivery change and explicitly admitted to execution by the Control Plane.
_Avoid_: Issue, task, PRD, spec

**Ticket Session（票据会话）**:
The durable context and responsibility chain exclusively assigned to an Executable Ticket until its delivery succeeds or is cancelled.
_Avoid_: Worker Run, container, controller-managed context packet

**Ticket Workspace（票据工作区）**:
The isolated, persistent version-control workspace owned by one Ticket Session and used by its successive Worker Runs.
_Avoid_: Container filesystem, shared checkout, dependency cache

**Ticket Agent（票据 Agent）**:
The sole agent identity currently accountable for a Ticket Session and responsible for initial implementation and every subsequent review revision; responsibility may transfer only after the prior session is confirmed permanently unrecoverable.
_Avoid_: Worker Run, replacement worker, coordinating agent

**Control Plane（控制平面）**:
The durable authority that schedules work and records execution ownership, leases, attempts, and runtime state.
_Avoid_: Scheduler agent, coordinating agent, Agent/rules

**Delivery Controller（交付控制器）**:
The component that owns one Executable Ticket's validation, publication requests, review delivery, and integration-check lifecycle.
_Avoid_: Ticket Agent, GitHub Actions, workflow scheduler

**GitHub Write Gateway（GitHub 写入网关）**:
The narrow boundary that validates a current Run Lease, ticket ownership, and expected remote revision before executing an external repository mutation.
_Avoid_: General GitHub proxy, agent credential, merge authority

**Gateway Credential（网关凭据）**:
The owner-wide fine-grained GitHub credential held only by the trusted GitHub Write Gateway to perform ticket-scoped branch, pull-request, issue, and CI operations across repositories admitted by the Control Plane.
_Avoid_: Worker token, package publisher token, general GitHub credential

**Delivery Cycle（交付周期）**:
The single long-lived validation and review lifecycle bound to an Executable Ticket until its delivery succeeds or is cancelled.
_Avoid_: Worker Run, validation run, pull request

**Revision Round（修订轮次）**:
One ordered candidate-and-validation round within a Delivery Cycle, created for the initial implementation or in response to new feedback.
_Avoid_: Delivery Cycle, Worker Run, new pull request

**Worker Run（执行单元）**:
A numbered, bounded execution episode in which the Ticket Agent actively works on its Executable Ticket.
_Avoid_: Ticket Agent, agent session, Docker task

**Worker Image Release（Worker 镜像发布）**:
An approved, source-keyed immutable version of the workflow platform's execution environment, published only from an owner-accepted commit on `main` when the Worker toolchain changes and consumed by later Worker Runs.
_Avoid_: Executable Ticket delivery, Candidate Revision, application deployment

**Active Worker Image（生效 Worker 镜像）**:
The latest Worker Image Release from an owner-accepted `main` commit that passed production doctor checks and is selected for new Worker Runs; existing runs remain pinned to their starting image.
_Avoid_: Published image, candidate image, mutable latest tag

**Worker Release Manifest（Worker 发布清单）**:
The machine-readable, sole GitHub Release asset that binds an accepted source commit, build-input identity, and pinned toolchain to the exact published GHCR digest for cross-host recovery and verification.
_Avoid_: Toolchain source config, local Docker metadata, Active Worker Image state

**Run Lease（运行租约）**:
A time-bounded, fenced authorization identifying the only Worker Run whose Candidate Revision may currently be accepted for an Executable Ticket.
_Avoid_: Assignment, lock, container lifetime

**Candidate Revision（候选修订）**:
An immutable revision and its verification evidence proposed by a Worker Run for publication.
_Avoid_: Pull request, final result, agent push

**Review Feedback（评审反馈）**:
A deduplicated review or delivery conversation written by a human collaborator and routed to the accountable Ticket Agent for interpretation.
_Avoid_: Mandatory change request, bot comment, CI result

**Merge-Ready（等待合并）**:
The non-terminal state in which the current proposed revision and its required integration checks have passed and an authorized human reviewer alone may accept it; new feedback or a relevant revision change invalidates the state.
_Avoid_: Delivered, approved forever, auto-merge

**Owner-Guarded Mode（所有者约束模式）**:
A repository assurance mode in which GitHub does not enforce required checks or reviews and the sole authorized owner preserves the delivery contract by merging only Merge-Ready revisions.
_Avoid_: Enforced Mode, branch-protected repository, unsafe mode

**Cancelled（已取消）**:
The human-authorized outcome that a Delivery Plan or an unmerged delivery will no longer proceed.
_Avoid_: Failed, expired, closed without a decision, abandoned

**Plan Amendment（计划修订）**:
A proposed change to an active Delivery Plan's membership, acceptance criteria, dependency edges, or cross-ticket contracts, applied only after human approval.
_Avoid_: Ticket implementation choice, agent-created blocker, silent scope expansion

**Needs Attention（需要介入）**:
The paused state reached when bounded automated recovery cannot make progress and a human decision or corrective action is required.
_Avoid_: Failed attempt, active retry, cancellation

**Delivered（已交付）**:
The successful outcome of an Executable Ticket whose accepted revision is confirmed present in the target integration line.
_Avoid_: Checks passed, pull request closed, issue closed
