---
status: accepted
---

# Trust one immutable Windows Installation Bundle

Each stable Platform Release publishes exactly one
`workflow-windows-amd64.zip`. Immutable GitHub Release metadata and its asset
SHA-256 authenticate the archive; root `platform-release.json` binds exact
version and complete payload inventory. No external manifest, SHA256SUMS, or
managed signing key is used.
