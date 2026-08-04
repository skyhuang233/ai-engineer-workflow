---
status: superseded by ADR-0032
---

# Historical private-repository exception

This exception is retired. The production baseline requires public repositories under ADR-0032; the owner remains the only human authorized to merge accepted pull requests.

## Consequences

Worker containers still receive no GitHub write credential, the Gateway exposes neither direct-`main` nor merge operations, and only the authorized owner may merge.
