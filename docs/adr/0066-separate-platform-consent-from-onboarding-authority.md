---
status: accepted
---

# Separate platform consent from onboarding authority

Workflow Setup uses one readable Platform Setup Consent for disclosed
host-facing changes and reserves an immutable Onboarding Plan Digest for
repository and GitHub mutations. Bundle-owned deterministic migration, pins,
launch, retry, and recovery do not require replacement platform digests, but a
new undisclosed host action requires consent before execution.
