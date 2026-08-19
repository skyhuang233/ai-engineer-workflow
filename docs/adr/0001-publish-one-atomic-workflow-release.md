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
final Worker SBOM and Grype scan, and consumers use only the final immutable
Worker digest carried by the Workflow Release.

The GitHub CLI remains a `version` plus exact Linux amd64 binary-hash pin in the
manifest. When its latest upstream release artifact cannot pass the required
Grype policy, the Worker may reproducibly rebuild that exact release commit with
the pinned Go toolchain and a fixed vulnerable Go dependency; the configured
binary hash must match before the image is admitted. This does not add another
release or provenance contract.
