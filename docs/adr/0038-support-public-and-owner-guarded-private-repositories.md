---
status: accepted
---

# Support public and Owner-Guarded private repositories

Repositories operated by this workflow may be public or private. Visibility is not an admission or assurance boundary. A project repository, the dedicated integration repository, and the Worker Release source repository are admitted only when their repository owner matches the configured Control Plane GitHub App owner. The same owner must perform the final non-bot pull-request merge. This supersedes the public-only production baseline in ADR-0032 and the historical private exception in ADR-0033.

Both visibility paths use Owner-Guarded Mode. GitHub branch protection, required checks, and merge queues may strengthen the contract where available, but are not prerequisites. The owner preserves the delivery contract by merging only Merge-Ready revisions. Worker containers receive no GitHub write credential; the Gateway exposes no merge or direct-`main` operation and continues to validate ticket-derived repository, branch, revision, and lease authority before each mutation.

## Consequences

Repository visibility never makes GitHub input trusted. Plan roots, plan membership and dependency inputs, pull-request feedback, projection observations, and Workflow Inbox answers are accepted only from the configured owner and never from bot accounts. Other repository input remains observation-only in both public and private repositories.

Credential provisioning performs the same live branch, issue, label, pull-request, comment, and cleanup contract against the configured integration repository regardless of visibility. Publisher and Doctor accept an owner-controlled private Worker Release repository but retain the same-repository, owner-merge, successful-main-workflow, immutable digest, release-manifest, and build-input provenance checks. GHCR package accessibility remains a separate artifact contract: Doctor must still resolve the pinned digest. The configured no-mistakes upstream and fork provenance repositories remain public under the existing supply-chain verification contract; changing that is a separate decision.
