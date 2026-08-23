# Authority and Safety

## Before any operation

- Require a valid Repository Admission and the repository's exact managed contract.
- Obey the repository's `AGENTS.md`, tracker routing, domain documents, and required checks.
- Separate human intent authority from runtime facts. Labels and status projections do not grant authority by themselves.

## Fencing

Ticket Agents and Delivery Controllers never receive raw GitHub write credentials. External writes use the GitHub Write Gateway with the current time-bounded Run Lease, ticket ownership, permitted branch/pull request, and expected remote head. A stale, cancelled, expired, or replaced run may leave diagnostics but must not mutate or become accepted.

Do not write directly to the default branch, bypass the Gateway, widen a lease, reuse another ticket's authority, or merge a delivery pull request.

## Human decisions

Route clarifications, `ask-user` findings, Plan Amendments, cancellation, closed-unmerged impact, recovery, and requests to skip a quality step to the stable Workflow Inbox question. Never answer on the user's behalf. Autonomous fixes are limited to explicitly allowed `auto-fix` or informational `no-op` outcomes within configured bounds.

## Amendments, pauses, and cancellation

An approved Plan Amendment may change only its displayed membership/dependency/contract delta; stale approval cannot authorize a different graph. Pause affected work while the decision is open. Confirmed cancellation terminates unfinished work and invalidates its Run Lease and mutation authority, but does not revert merged work or unblock cross-plan dependents automatically.

On any authority ambiguity or fencing conflict, stop the mutation, preserve evidence, and report the exact blocker or Workflow Inbox question id.
