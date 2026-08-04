---
status: accepted
---

# Record Worker digests in release manifests

The exact GHCR digest cannot exist in the pull request that authorizes a Worker toolchain change because it is produced only after that commit enters `main`. The post-merge publisher therefore attaches a machine-readable `worker-release.json` to the corresponding GitHub Worker Release, binding the accepted source commit, Worker version, immutable image digest, pinned Codex and `no-mistakes` versions, upstream commit, fork release, release-asset checksum, and Actions run. Git remains authoritative for build inputs, the Worker Release Manifest is authoritative for the cross-host build output, and SQLite records only which verified release is the current host's Active Worker Image.
