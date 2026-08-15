---
name: agent-workflow
description: Execute the installed Agent Workflow contracts for plans, tickets, review, delivery, and human inbox coordination.
---

# Agent Workflow

Use the repository's managed Agent instructions and `.workflow/repository.json` as repository-specific configuration. Use GitHub Issues as the collaboration control surface. Never bypass Control Plane admission, write-gateway fencing, approval digests, required checks, or repository-owned instructions.

When a workflow skill creates or activates a Delivery Plan Root, immediately bind that root to the admitted repository before reporting success:

```powershell
workflow runtime-configure --source (git rev-parse --show-toplevel) --root <plan-root-issue-number>
```

This is automatically performed Workflow bookkeeping, not a user step or approval. Run it only after the Plan Root and its ticket graph have been published successfully. If binding fails, report the Plan Root operation as incomplete and do not claim that execution has started.
