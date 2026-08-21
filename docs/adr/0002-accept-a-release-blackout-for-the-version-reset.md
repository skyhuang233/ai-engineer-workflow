# Accept a release blackout for the version reset

The repository will disable its legacy component publishers and delete every
legacy GitHub Release and remote release tag before `workflow-v0.0.1` is ready.
This intentionally makes fresh installation and some release-dependent recovery
unavailable while known bugs and the atomic publisher are corrected, rather
than continuing to present releases the owner does not consider usable.
All legacy `workflow-worker` GHCR image versions are deleted in the same reset;
the owner accepts loss of existing-run recovery and historical reproducibility.

## Git Flow bootstrap exception

For `workflow-v0.0.1` only, the repository owner accepts the existing
`release-0.0.1` ancestry even though the branch was not cut from a live
`develop` head. Recutting the already-qualified first release is not required.

All remaining Git Flow boundaries still apply. The owner must merge the release
through separate pull requests to `main` and `develop`, and only the unified
publisher may create the immutable tag and GitHub Release after the `main`
merge. Agents must not merge either pull request.

After this bootstrap release, every release branch must start from the live
`develop` head and no part of this exception carries forward.
