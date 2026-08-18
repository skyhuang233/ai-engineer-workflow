---
status: accepted
---

# Use a bounded maintenance window for platform upgrades

An upgrade first reads active work through Dispatcher. Under an exclusive
Workflow Home lock, target Launcher writes a target-bound maintenance fence and
counts active runs in one transaction; scheduler claims check that fence. Zero
work permits stop, snapshot, migration, and one candidate activation. A
post-activation failure remains fenced for forward repair; Setup never cancels
or waits for Worker Runs.
