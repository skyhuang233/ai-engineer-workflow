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

## First-release qualification exception

`workflow-v0.0.1` reuses the full functional qualification completed for the
exact candidate commit `b35d239f4fe7e4ed55cb800942b2a36cf7468058`, including
the clean Setup flow, owner-merged Onboarding and Worker pull requests, and the
resulting Ticket Delivered and Plan Completed evidence in the approved short
repository fixture. Its release workflow must continuously revalidate that
evidence and must reject any product-source difference, Bundle digest, or Worker
image digest from the approved candidate. Only the release-CI proof and its
regression test may differ. This exception exists solely to publish the already
qualified first release without rerunning its disposable-host qualification.

Every later release and hotfix must run the complete qualification harness
against its own exact candidate; this exception does not relax any future
qualification requirement.

After this bootstrap release, every release branch must start from the live
`develop` head and no part of this exception carries forward.
