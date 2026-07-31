---
status: accepted
---

# Allow owner-guarded private repositories

The production baseline permits private repositories without GitHub-enforced branch protection when one authorized owner accepts responsibility for merging only Merge-Ready revisions. This Owner-Guarded Mode preserves privacy and avoids making a paid GitHub plan a runtime prerequisite, while deliberately accepting that required checks, renewed review after revisions, and the pull-request-only path are enforced by the GitHub Write Gateway, `no-mistakes`, current-head validation, and owner discipline rather than by GitHub itself.

## Consequences

The absence of branch protection is a normal supported production state in Owner-Guarded Mode, so `workflow doctor` neither warns nor fails for it. Worker containers still receive no GitHub write credential, the Gateway still exposes neither direct-`main` nor merge operations, and only the authorized owner may merge. Repository visibility is a project decision rather than a workflow requirement: public and private repositories are both supported, and repositories are never made public merely to obtain free branch protection. ADR-0032 is superseded.
