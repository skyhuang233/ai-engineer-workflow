---
status: accepted
---

# Use public repositories as the production baseline

All repositories operated by this workflow are public so GitHub Free can enforce required checks and human-review branch protection without a paid private-repository plan. This deliberately trades source, issue, pull-request, workflow-log, and history confidentiality for a zero-cost server-enforced integration boundary; secrets and privileged control-plane state must remain outside repository-visible surfaces.

## Consequences

The private-repository threat-model assumption in ADR-0018 no longer applies. Public comments, reviews, pull requests, forks, and other repository events are untrusted external input even when the intended day-to-day user is the repository owner, so their authority to wake or instruct a Ticket Agent must be decided explicitly before autonomous execution is enabled.
