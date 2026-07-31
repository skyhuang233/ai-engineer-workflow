# Workflow doctor report

Generated: 2026-07-31T05:56:20Z

| Check | Status | Evidence |
|---|---|---|
| Codex CLI | PASS | codex-cli 0.146.0 |
| no-mistakes CLI | PASS | no-mistakes version v1.41.2 (867d64d) 2026-07-24T06:16:02Z |
| Codex session resume | PASS | persistent session ID created and resumed successfully |
| SQLite durability and locking | PASS | WAL, synchronous=FULL, foreign keys, integrity, and serialized writer locking verified |
| Docker Worker contract | PASS | Linux Engine, bind mounts, host.docker.internal Gateway, pinned tools, and absence of GitHub write credentials verified |
| Published Worker digest | FAIL | registry cannot resolve pinned Worker digest: exit status 1 (Get "https://ghcr.io/v2/skyhuang233/workflow-worker/manifests/0.1.0": denied) |
| GitHub credential scope | FAIL | least-privilege credential has not been human-attested |
| GitHub protected integration contract | FAIL | branch protection unavailable: exit status 1 ({"message":"Upgrade to GitHub Pro or make this repository public to enable this feature.","documentation_url":"https://docs.github.com/rest/branches/branch-protection#get-branch-protection","status":"403"}gh: Upgrade to GitHub Pro or make this repository public to enable this feature. (HTTP 403)) |
