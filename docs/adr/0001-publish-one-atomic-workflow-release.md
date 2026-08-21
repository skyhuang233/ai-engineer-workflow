# Publish one atomic Workflow Release

Platform and Worker will share one version and one immutable Workflow Release
instead of publishing separate artifact and tag lines. This makes compatibility
explicit and gives each Git Flow release branch one unambiguous product outcome,
at the cost of rebuilding and publishing both components whenever either one
changes.

The Workflow Release is also the supply-chain publication boundary. Worker
tools are recorded in its manifest, and no-mistakes is pinned by repository,
full commit, and version, then compiled from a clean checkout with the pinned Go
toolchain. The design intentionally has no second no-mistakes Release lifecycle
and no canonical Git-tree/blob build identity. Publication remains gated on the
final Worker SBOM, and consumers use only the final immutable Worker digest
carried by the Workflow Release. The high-severity, only-fixed Grype scan is
advisory and non-blocking for functional release qualification: findings or a
scanner failure do not prevent candidate preservation or publication. This is
an explicit containment while security review is out of scope, not evidence
that the candidate passed a vulnerability gate.

After functional qualification, the exact Bundle, manifest, and SBOM are
stored together in a scratch OCI image. Its discovery tag is keyed by the full
candidate head, qualification attempt, and manifest SHA-256, while image labels
also bind the qualification run ID. The publisher discovers the matching tag
but pulls the package version by immutable registry digest, rechecks every
binding, and then reverifies the three candidate files. A rerun creates a new
attempt-bound identity and cannot hide the candidate accepted by an earlier
successful attempt.

The GitHub CLI remains a `version` plus exact Linux amd64 binary-hash pin in the
manifest. When its latest upstream release artifact cannot pass the required
Grype policy, the Worker may reproducibly rebuild that exact release commit with
the pinned Go toolchain and a fixed vulnerable Go dependency; the configured
binary hash must match before the image is admitted. This does not add another
release or provenance contract.
