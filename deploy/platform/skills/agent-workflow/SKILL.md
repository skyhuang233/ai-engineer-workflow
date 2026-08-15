---
name: agent-workflow
description: Operate an admitted Agent Workflow repository through Delivery Plans, Executable Tickets, Workflow Inbox decisions, pull-request delivery, amendments, and cancellation. Use when Codex must plan, activate, monitor, review, answer, repair, or cancel work managed by Agent Workflow.
---

# Agent Workflow

Read the repository's `AGENTS.md` managed block and `.workflow/repository.json` first. Follow `docs/agents/issue-tracker.md` for repository-specific tracker commands and `docs/agents/domain.md` for domain-document routing. Do not copy configuration from this skill into the repository.

Load only the references needed for the operation. For every GitHub read or write, load `references/github.md` in addition to the operation-specific reference:

- Resolve GitHub identity and perform deterministic, idempotent reads/writes: [references/github.md](references/github.md)
- Create, approve, activate, inspect, or amend a Delivery Plan: [references/plans.md](references/plans.md)
- Publish or relate Executable Tickets: [references/tickets.md](references/tickets.md)
- Read or answer the Workflow Inbox: [references/inbox.md](references/inbox.md)
- Inspect or respond to delivery pull requests: [references/pull-requests.md](references/pull-requests.md)
- Decide whether an operation is authorized, fenced, paused, or cancelled: [references/authority.md](references/authority.md)

Never bypass Repository Admission, approval boundaries, GitHub Write Gateway fencing, required checks, or repository-owned instructions. Never merge a delivery pull request for the user.

After the fully published ticket graph for a newly created or newly activated Plan Root is ready, automatically bind it before reporting success:

```powershell
workflow runtime-configure --source (git rev-parse --show-toplevel) --root <plan-root-issue-number>
```

This is Workflow bookkeeping, not a user step or approval. If publication, activation, or binding fails, report the Plan Root operation as incomplete and do not claim execution started.

The Workflow CLI resolves ChatGPT authentication through `codex doctor --json` and verifies `codex login status`; it must not ask the user to locate a private Codex file.
