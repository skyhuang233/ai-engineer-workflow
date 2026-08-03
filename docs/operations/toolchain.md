# Production toolchain baseline

`config/toolchain.json` is the machine-readable production baseline checked by
`workflow doctor`. Every executable version and artifact is immutable:

- Codex CLI is pinned to an exact package version.
- `no-mistakes` is pinned to an upstream release, verified commit, fork
  repository, fork release, and Linux release-asset checksum.
- The Worker image is pinned to a registry manifest digest and is built from a
  base image digest. The separately recorded local build ID is only for
  pre-publication host probes; registry resolution is an independent mandatory
  check.
- The dedicated GitHub integration repository and its required status check
  and human review count are explicit.
- The Gateway fine-grained PAT or GitHub App is declared with a private
  repository allowlist and exact permissions. A human must fill
  `approved_by`, `approved_at`, and the SHA-256 fingerprint of the credential
  only after comparing those declarations with GitHub's credential settings.
  The fingerprint binds the attestation to the active secret without recording
  the secret itself.

Run the complete target-host contract with:

```powershell
go run ./cmd/workflow doctor `
  --config config/toolchain.json `
  --database C:\tmp\workflow-doctor.db `
  --report docs/operations/doctor-report.md
```

`workflow run-ticket` is the production Worker-to-Gateway path. It runs the
pinned Docker Worker without the GitHub credential, accepts its candidate
commit, then submits and dispatches the candidate push and pull-request
commands through the credential-owning Gateway. `workflow reconcile-delivered`
checks merged pull requests for reachability from `main` before marking their
tickets Delivered.

The command fails closed if any check fails. In particular, a locally built
image is not evidence of publication: the pinned digest must resolve from the
registry. The report must be reviewed before filling the credential
attestation fields.

## Upgrade rule

Never edit only one version string. A toolchain upgrade is accepted only after:

1. recording the new upstream release and full verified commit;
2. publishing a new immutable fork release;
3. verifying release-asset checksums before use;
4. rebuilding and publishing the Worker image under a new version;
5. replacing the image reference with the registry-reported digest;
6. running unit, Docker, Codex resume, SQLite, Gateway, and dedicated GitHub
   contract checks; and
7. committing a fresh redacted `workflow doctor` report.

Floating tags such as `latest`, floating branches such as `main`, and
unversioned local executables are not production inputs.
