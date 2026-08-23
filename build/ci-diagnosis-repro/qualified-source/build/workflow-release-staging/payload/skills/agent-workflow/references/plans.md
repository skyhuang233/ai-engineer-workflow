# Delivery Plans

## Inspect before writing

Read the repository tracker and domain routing instructions. Resolve the admitted repository, then inspect existing `workflow:plan` roots, their native sub-issues, dependency edges, labels, comments, and current Workflow Inbox questions. Do not infer approval or activation from prose or a closed issue.

## Create and approve

1. Turn the approved intended outcome into one non-executable Plan Root. Give it the `workflow:plan` label, never `workflow:ticket`, and record scope, acceptance criteria, constraints, and evidence expected at completion.
2. Propose the complete Executable Ticket graph before publishing it. Ask the human to approve changes to intent, membership, dependencies, or cross-ticket contracts.
3. After approval, publish every ticket and relationship as described in `tickets.md`. A partial graph remains inactive.
4. Add `workflow:active` only after the entire approved graph exists. Do not invent another activation label or confirmation step.
5. Automatically run `workflow runtime-configure --source (git rev-parse --show-toplevel) --root <plan-root-issue-number>` after successful publication/activation. A failed binding makes the operation incomplete.

## Inspect status

Treat the Plan Root status projection as a view. Verify native tickets, dependencies, delivery facts, and open Workflow Inbox questions before summarizing Building, Active, Paused, Needs Attention, Completed, or Cancelled.

## Amend or cancel

An active Plan's intent, membership, dependency edges, and cross-ticket contracts change only through a Plan Amendment with impact analysis and explicit human approval in the Workflow Inbox. Pause only the affected ticket and downstream subgraph while approval is pending.

For cancellation, show unfinished tickets, pull requests, merged work, and cross-plan dependents. Require the specific Workflow Inbox decision. Cancellation stops unfinished work but never reverts merged changes automatically.
