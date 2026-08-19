# Agent Workflow

Agent Workflow is the product developed and released by this repository.

## Language

**Workflow Release**:
One versioned product release that delivers a mutually compatible Platform and
Worker as a single unit.
_Avoid_: Platform Release, Worker Release, component release

**Worker Tool Pin**:
The version and immutable source or asset coordinates used to build a tool into
the Worker. A no-mistakes pin is exactly repository, full commit, and version;
it is not a separate Release provenance chain. The GitHub CLI consumer contract
remains version plus exact binary hash even when the Worker rebuilds its release
commit to apply a security-fixed Go dependency.
_Avoid_: no-mistakes Release, fork Release provenance, build-input identity
