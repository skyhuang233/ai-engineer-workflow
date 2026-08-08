# Seed Ticket Sessions from the host ChatGPT authentication cache

The single-host Control Plane uses the trusted operator's existing Codex ChatGPT login to authenticate Ticket Agents. `doctor`, `run-ticket`, and `poll-github` accept an absolute `--codex-auth-file`, defaulting to `CODEX_HOME/auth.json` or the current user's `.codex/auth.json`. Admission validates that the source is a regular ChatGPT login cache before any ticket is claimed or Worker is launched.

On the first Worker Run for a Ticket Session, the Control Plane atomically copies the source to that Session's private `CODEX_HOME/auth.json`. Once present, the Session copy is authoritative: Codex may refresh it in place and later Worker Runs must not overwrite it from the host source. The cache is never stored in Git, SQLite, reports, logs, diagnostic bundles, environment variables, or the Ticket Workspace. It is retained and reclaimed with the Ticket Session's Codex state.

If a Worker leaves that Session cache malformed or unreadable, the Control Plane cannot safely redact detailed evidence. It therefore records only a credential-free minimal diagnostic and still durably marks the Run failed; raw Worker output, diffs, status, and residue are omitted rather than risking credential disclosure or losing the terminal Run record.

This deliberately gives the trusted Ticket Agent access to the model credential it needs to operate. Protecting the credential from arbitrary code already executing as that trusted Worker is outside the accepted single-user threat model. GitHub write credentials remain excluded from Workers and fenced through the GitHub Write Gateway; this decision does not weaken that separate boundary.

`workflow doctor` copies the same source into a temporary state directory and uses the pinned Worker image to create and resume a real Codex session. A missing, malformed, non-ChatGPT, expired, or rejected cache fails the production contract. The temporary cache is removed after the probe. This supersedes version-only Worker Codex validation as sufficient evidence of readiness.

API-key authentication remains deliberately excluded: it would use separate Platform billing and is not the operator-selected identity for this workflow. Reconsider this decision if the Control Plane becomes multi-user, Workers become untrusted, or concurrent ChatGPT cache refresh proves unreliable.
