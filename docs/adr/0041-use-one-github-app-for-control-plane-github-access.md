---
status: accepted
---

# Use one GitHub App for Control Plane GitHub access

The trusted single-host Control Plane uses one GitHub App installed for all repositories owned by the configured account. Its minimum repository permissions are Metadata read, Actions read, Checks read, Contents write, Issues write, and Pull requests write. The same installation credential serves host-side GitHub observations, including check runs, and the GitHub Write Gateway's fenced mutations. This supersedes ADR-0036 because github.com does not currently offer the Checks permission required by this workflow in the fine-grained PAT creation UI, so the previous `fine-grained-pat + Checks: read` contract cannot be provisioned reliably.

The App private key is a fixed PEM file on the trusted Windows host. Provisioning receives the non-secret App ID, authenticates with the PEM, discovers the installation for the integration repository, and requires the installation to belong to the configured owner and cover all repositories. It then creates an installation token, verifies the declared permissions, and runs the existing live Actions, Git push, check-runs, issue, label, pull-request, comment, and cleanup contract. SQLite stores only the App ID, installation ID, PEM SHA-256 fingerprint, owner, integration repository, and verification time. It never stores the PEM or an installation token.

Each long-running host process may cache its installation token and refresh it shortly before GitHub's mandatory expiry. There is no additional per-repository token narrowing, per-purpose App, local one-hour policy, Windows Credential Manager indirection, or confirmation prompt. These restrictions would add operational work without changing the trusted-host boundary. The owner accepts the App installation's owner-wide reach; repository admission, Ticket Session authority, Run Leases, expected revisions, and the Gateway interface remain the controls on what the product may do.

Worker containers and embedded Delivery Controllers never receive the App private key, App JWT, installation ID, or installation token. This boundary is retained because Workers have no direct GitHub mutation responsibility and removing it would bypass the Gateway's core authority checks, not merely reduce defensive hardening.

Provisioning retains the durable rotation fence: new Gateway writes pause, expired delivery claims are recovered, in-flight writes quiesce, and writes resume only after installation discovery, permission validation, and the complete live contract succeed. A missing, changed, revoked, expired, or rejected App credential pauses Gateway writes as Needs Attention without terminating Worker work or discarding durable state. Routine Doctor runs verify that the PEM fingerprint and installation identity still match, mint a current installation token, and exercise the read-only production checks; they do not create recurring contract pull requests.
