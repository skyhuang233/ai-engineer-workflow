---
status: accepted
---

# Use public repositories as the production baseline

All repositories operated by this workflow are public. This deliberately trades source, issue, pull-request, workflow-log, and history confidentiality for a production baseline that works on GitHub Free; secrets and privileged control-plane state must remain outside repository-visible surfaces. Branch protection and merge queues may strengthen that baseline where available, but they are not prerequisites: final delivery still requires the configured owner to perform a non-bot pull-request merge.

## Consequences

The private-repository threat-model assumption in ADR-0018 no longer applies. Public comments, reviews, pull requests, forks, and other repository events are untrusted external input even when the intended day-to-day user is the repository owner. The Control Plane accepts plan roots, plan membership and dependency inputs, pull-request feedback, projection observations, and Workflow Inbox answers only from the configured repository owner and never from bot accounts; all other repository input is observation-only.
