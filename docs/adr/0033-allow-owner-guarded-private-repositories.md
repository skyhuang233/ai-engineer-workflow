---
status: superseded by ADR-0038
---

# Historical private-repository exception

ADR-0032 retired this exception in favor of a public-only baseline. ADR-0038 later replaced both decisions with one visibility-neutral Owner-Guarded baseline; the owner remains the only human authorized to merge accepted pull requests.

## Consequences

Worker containers still receive no GitHub write credential, the Gateway exposes neither direct-`main` nor merge operations, and only the authorized owner may merge.
