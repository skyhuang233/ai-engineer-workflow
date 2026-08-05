---
status: accepted
---

# Activate verified Worker releases without a second approval

Merging a Worker-toolchain pull request into `main` is the sole human approval for both its Worker Image Release and its use by future Worker Runs. After the accepted commit is published to GHCR, successful production doctor checks atomically make its immutable digest the Active Worker Image; build, publication, or doctor failure leaves the prior digest active, and already-running Worker Runs remain pinned to the digest with which they started. A second digest-only approval would duplicate the owner's merge decision without adding a distinct trust boundary.
