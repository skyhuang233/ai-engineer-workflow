# Accept a release blackout for the version reset

The repository will disable its legacy component publishers and delete every
legacy GitHub Release and remote release tag before `workflow-v0.0.1` is ready.
This intentionally makes fresh installation and some release-dependent recovery
unavailable while known bugs and the atomic publisher are corrected, rather
than continuing to present releases the owner does not consider usable.
All legacy `workflow-worker` GHCR image versions are deleted in the same reset;
the owner accepts loss of existing-run recovery and historical reproducibility.
