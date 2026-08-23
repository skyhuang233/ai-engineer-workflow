# Executable Tickets

## Publish the graph

For every approved work item:

1. Create one issue labelled `workflow:ticket`; do not add `workflow:plan`.
2. Scope it to one independently reviewable delivery change with explicit acceptance criteria, validation evidence, and relevant domain terms.
3. Attach it to the Plan Root as a native sub-issue. Plan membership and execution dependencies are separate facts.
4. Create each approved native blocked-by dependency explicitly. Never infer dependencies from issue order, file hints, or prose.
5. Read the complete graph back. Only then activate the root and perform the automatic runtime binding from `plans.md`.

Use the repository's `docs/agents/issue-tracker.md` commands for issue, sub-issue, and dependency operations. Do not replace native relationships with a new label vocabulary when the tracker supports them.

## Runtime meaning

The Control Plane, not this skill, admits the deterministic executable frontier. A ticket is eligible only while its Plan is active, all blockers are Delivered, no Ticket Agent owns it, and runtime capacity exists. `workflow:delivered`, issue closure, or checks passing are projections, not independent delivery authority; delivery requires the accepted pull-request revision to be merged and reachable from the target integration branch.

If one ticket cannot fit one persistent branch and pull request, split it before activation. Changes discovered after activation that alter graph intent require a Plan Amendment rather than silent ticket edits.
