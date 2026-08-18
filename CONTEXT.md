# Agent Workflow

This context describes the language used to plan, coordinate, and deliver software changes through autonomous agents.

## Language

**Bootstrap Development Harness（引导开发工具链）**:
The repository-external legacy agent workflow used locally to develop Agent Workflow until the product is complete; it has no authority over product runtime design or behavior.
_Avoid_: Control Plane, product workflow, production entry point

**Self-Hosting Cutover（自托管切换）**:
The human-approved transition from the Bootstrap Development Harness to developing Agent Workflow through its own product workflow, permitted only after independent end-to-end production validation.
_Avoid_: automatic rollout, release publication, completion inferred from closed issues or passing tests

## Windows platform installation

**Workflow Setup（Workflow 配置）**:
The human-invoked, Codex-led operation that prepares one current-user Windows host and then onboards the current repository.
_Avoid_: remote-host deployment, user-run installation checklist

**Platform Setup Consent（平台配置同意）**:
The readable confirmation of disclosed host-facing installation changes. It authorizes Bundle-owned deterministic installation and recovery but is not a plan digest.
_Avoid_: Platform Bootstrap Plan, Onboarding Plan

**Platform Setup Consent Record（平台配置同意记录）**:
The immutable Launcher-owned audit record of an accepted target and concrete capabilities, referenced by Attempts and the active installation.
_Avoid_: approval digest, mutable permission set

**Onboarding Plan Digest（接入计划摘要）**:
The SHA-256 identity of a canonical immutable Onboarding Plan. Repository and GitHub mutations bind to this exact digest.
_Avoid_: Platform Setup Consent

**Workflow Bootstrap Skill（Workflow 引导技能）**:
The independently installed Codex skill that validates the current repository and login, obtains one Bundle, narrates consent, invokes Launcher protocol, then drives Repository Onboarding.
_Avoid_: versioned Workflow CLI, platform planner

**Windows Installation Bundle（Windows 安装包）**:
The sole `workflow-windows-amd64.zip` platform download carrying root Platform Release Manifest, dual-mode Setup Launcher/Dispatcher, versioned CLI, skill bundle, and repository contract.
_Avoid_: split Release assets, loose checksum file

**Setup Launcher（配置启动器）**:
The Bundle executable that exclusively owns platform inspection, consent, installation, migration, activation, verification, and forward repair. Installed as `bin\\workflow.exe`, its same bytes implement Dispatcher-only delegation.
_Avoid_: PowerShell planner, installed-CLI installer

**Setup Launcher Protocol（配置启动器协议）**:
The schema-versioned one-JSON-stdin/one-JSON-stdout `inspect`, `apply`, and `verify` contract between Bootstrap Skill and Launcher.
_Avoid_: Platform Setup Contract, Platform Bootstrap Plan

**Platform Release Manifest（平台发布清单）**:
The root Bundle metadata binding exact version, compatibility contract, inventory, and per-file digests after GitHub immutable asset metadata authenticates the archive.
_Avoid_: external manifest, SHA256SUMS

**Workflow Home（Workflow 主目录）**:
The one current-user local root for immutable platform generations, shared credentials, workspaces, logs, and backups.
_Avoid_: repository root, ProgramData

**Platform Installation（平台安装）**:
The one active platform represented solely by atomic `platform/active.json`, identifying active Generation, Attempt, Consent, version, and `repair_required` or `ready` readiness.
_Avoid_: partial installation, Platform Generation

**Platform Generation（平台代际）**:
One bundle-digest-named immutable payload slot with its generation-local database; only an active Generation's database receives ordinary runtime writes.
_Avoid_: Windows Installation Bundle, shared Workflow Home state

**Workflow Dispatcher（Workflow 分派器）**:
The stable `workflow.exe` that validates only `active.json` and delegates ordinary commands to the exact versioned CLI or setup inspection/verification to its active Launcher.
_Avoid_: copied active CLI, setup mutator

**Platform Installation Attempt（平台安装尝试）**:
One candidate transition to one target Bundle under one Consent, staged through migrated, verified, activation-as-repair-required and ready, or failed only before activation.
_Avoid_: Platform Installation, Setup Execution Result

**Platform Maintenance Window（平台维护窗口）**:
The exclusive bounded install interval that writes a target-bound scheduling fence, proves zero active Worker Runs, migrates one candidate, and activates it. Post-activation failure repairs forward without rollback.
_Avoid_: zero-downtime deployment, concurrent Control Planes

**Shared Credential Initialization（共享凭据初始化）**:
First setup's capture and, after consent, plaintext persistence of one owner-bound classic PAT for Release access and trusted Control Plane operations; Workers never receive it.
_Avoid_: separate Release credential, GitHub App

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

**Delivery Source（交付源）**:
The credential-free, read-only Git snapshot pinned when a Revision Round begins and retained through that round's Candidate delivery; it is refreshed from the host-side admitted repository only for a new Revision Round, distinguished from both the mutable host source checkout and the Ticket Workspace, and reclaimed after a later accepted round or the Ticket Session is closed.
_Avoid_: Host source checkout, Ticket Workspace, live remote clone

**Ticket Agent（票据 Agent）**:
The sole agent identity currently accountable for a Ticket Session and responsible for initial implementation and every subsequent review revision; responsibility may transfer only after the prior session is confirmed permanently unrecoverable.
_Avoid_: Worker Run, replacement worker, coordinating agent

**Codex Authentication Source（Codex 认证源）**:
The trusted host's ChatGPT login cache used only to seed a Ticket Session's private Codex state before its first Worker Run; the Session copy then refreshes and persists independently.
_Avoid_: Gateway Credential, Worker environment variable, repository secret

**Control Plane（控制平面）**:
The durable authority that schedules work and records execution ownership, leases, attempts, and runtime state.
_Avoid_: Scheduler agent, coordinating agent, Agent/rules

**Delivery Controller（交付控制器）**:
The component that owns one Executable Ticket's validation, publication requests, review delivery, and integration-check lifecycle.
_Avoid_: Ticket Agent, GitHub Actions, workflow scheduler

**GitHub Write Gateway（GitHub 写入网关）**:
The narrow boundary that validates a current Run Lease, ticket ownership, and expected remote revision before executing an external repository mutation.
_Avoid_: General GitHub proxy, agent credential, merge authority

**Control Plane GitHub App Credential（控制平面 GitHub App 凭据）**:
The owner-wide GitHub App installation credential used by trusted host-side GitHub readers and the GitHub Write Gateway across repositories admitted by the Control Plane. The App private key remains on the host and Workers never receive GitHub credentials.
_Avoid_: Gateway-only credential, fine-grained PAT, Worker token, package publisher token

**Delivery Cycle（交付周期）**:
The single long-lived validation and review lifecycle bound to an Executable Ticket until its delivery succeeds or is cancelled.
_Avoid_: Worker Run, validation run, pull request

**Revision Round（修订轮次）**:
One ordered candidate-and-validation round within a Delivery Cycle, created for the initial implementation or in response to new feedback.
_Avoid_: Delivery Cycle, Worker Run, new pull request

**Worker Run（执行单元）**:
A numbered, bounded execution episode in which a Ticket Agent or Delivery Controller operates under a fenced Run Lease for one Ticket Session.
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
A deduplicated review or delivery conversation written by the configured owner and routed to the accountable Ticket Agent for interpretation.
_Avoid_: Mandatory change request, bot comment, CI result

**Merge-Ready（等待合并）**:
The non-terminal state in which the current proposed revision and its required integration checks have passed and an authorized human reviewer alone may accept it; new feedback or a relevant revision change invalidates the state.
_Avoid_: Delivered, approved forever, auto-merge

**Owner-Guarded Mode（所有者约束模式）**:
A visibility-neutral repository assurance mode for public or private repositories in which GitHub does not enforce required checks or reviews and the sole authorized owner preserves the delivery contract by merging only Merge-Ready revisions.
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
