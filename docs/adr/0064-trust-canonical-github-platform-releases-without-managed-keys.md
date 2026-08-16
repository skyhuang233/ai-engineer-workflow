---
status: accepted
---

# Trust canonical GitHub Platform Releases without managed signing keys

The Workflow Bootstrap Skill treats the canonical GitHub repository and its
HTTPS/API release metadata as the Platform Release trust root. It accepts only
an immutable stable release with the fixed platform tag and asset set, exact
`target_commitish`, an owner-merged `main` source commit, and a successful
`.github/workflows/publish-platform.yml` run whose repository, commit and run id
match the manifest. The manifest, provenance subjects, package inventory, and
SHA-256/size readback bind every downloaded artifact before extraction.

Platform publication uses the job-scoped `GITHUB_TOKEN`. The platform maintains
no long-lived Platform Release public key, private key, signing secret, detached
signature, key-rotation command, or key ceremony. This removes a separate trust
system that blocked a freshly installed Workflow Bootstrap Skill before it
could produce a Platform Bootstrap Plan, while retaining fail-closed GitHub
identity, provenance, immutability, and content-integrity checks.

The classic GitHub PAT is a different credential boundary. It remains plaintext
under the current user's Workflow Home for trusted Setup, Control Plane, CLI,
and GitHub Write Gateway processes, and is verified through standard input,
fingerprint, live owner binding, scopes, and capabilities. It is never a
Platform Release input or asset and never enters command arguments, ordinary
logs, repository content, or Worker containers.
