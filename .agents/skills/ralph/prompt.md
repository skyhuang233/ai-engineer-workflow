# ISSUE SOURCE

The selected GitHub Issue is provided at the start of context. GitHub Issues
are the authoritative task source for this repo.

Do not read local `issues/` files as the active task queue. The local
`issues/` directory is historical archive only.

You will work on one bounded AFK implementation issue only. The wrapper has
already selected and claimed an open GitHub Issue labeled `ready-for-agent`,
without `prd`, `blocked`, or `in-progress` at selection time.

If all eligible AFK tasks are complete, output `<promise>NO MORE TASKS</promise>`.

# ISSUE STRUCTURE

The selected issue contract follows the `to-issues` structure. The AFK wrapper
may source that contract from the issue body or, when the body is incomplete,
from a structured issue comment:

- `## Parent`: optional parent issue or PRD context.
- `## What to build`: the implementation boundary.
- `## Acceptance criteria`: the stop condition.
- `## Blocked by`: blocking GitHub Issues or `None - can start immediately`.

Use the selected child issue as the execution boundary. Parent PRDs are context,
not permission to expand scope.

# TASK SELECTION

Do not pick a different task. Do not open a new issue unless the selected issue
or parent PRD explicitly asks you to do so.

If you discover follow-up work, mention it in the completion comment instead of
creating new queue items.

# EXPLORATION

Explore the repo enough to understand the selected issue and the existing code
shape. Use the project's glossary and ADR guidance when naming domain concepts
or changing behavior.

# IMPLEMENTATION

Use a test-driven loop where practical:

1. Reproduce or specify the expected behavior with a focused test or check.
2. Make the smallest implementation change that satisfies the selected issue.
3. Re-run the relevant verification.

Do not expand beyond the selected issue's acceptance criteria. If a larger
problem appears, record it as a follow-up note in the GitHub issue comment.

# FEEDBACK LOOPS

Run the smallest relevant verification commands for the files you changed. If a
standard full-project check is cheap and available, run it too.

If you cannot run a relevant check, state why in the GitHub completion comment.

# GIT

Make a git commit for the completed issue. The commit message must include:

1. The GitHub issue number.
2. Key decisions made.
3. Files changed.
4. Blockers or notes for next iteration, if any.

Commit only files relevant to the selected issue. Do not commit unrelated dirty
files or tool artifacts.

# GITHUB ISSUE UPDATE

GitHub writes are owned by the Control Plane and its credential-isolated
GitHub Write Gateway. Do not invoke `gh`, `git push`, `curl`, or any other
direct GitHub write command for comments, labels, issue closure, pull requests,
or pushes. Return local evidence to the Delivery Controller; the Control Plane
projects issue state and publishes accepted revisions through the fenced
Gateway.

When the task is complete:

1. Return a completion summary containing the change, verification, commit,
   and any follow-up suggestions.
2. Report the intended terminal issue state as `completed`.
3. Leave all comments, labels, issue closure, pushes, and pull requests to the
   Delivery Controller and fenced Gateway.

When the task is not complete:

1. Return a progress summary containing what was done and what remains.
2. Report the intended issue state as `blocked` or `needs-info` and include the
   concrete reason.
3. Leave all comments and label transitions to the Delivery Controller and
   fenced Gateway.

Do not move or edit local issue files.

# FINAL RULES

ONLY WORK ON THE SELECTED ISSUE.
