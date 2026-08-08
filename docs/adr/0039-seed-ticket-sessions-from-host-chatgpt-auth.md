# Seed Ticket Sessions from the host ChatGPT authentication cache

The single-host Control Plane uses the trusted operator's existing Codex ChatGPT login to authenticate Ticket Agents. `doctor`, `run-ticket`, and `poll-github` accept an absolute `--codex-auth-file`, defaulting to `CODEX_HOME/auth.json` or the current user's `.codex/auth.json`. Admission validates that the source is a regular ChatGPT login cache before any ticket is claimed or Worker is launched.

On the first Worker Run for a Ticket Session, the Control Plane atomically copies the source to that Session's private `CODEX_HOME/auth.json`. Once present, the Session copy is authoritative: Codex may refresh it in place and later Worker Runs must not overwrite it from the host source. It is retained and reclaimed with the Ticket Session's Codex state.

Ticket Agents and their Worker code are trusted in this private, single-user workflow. The workflow does not attempt to prevent them from copying Codex credentials into Git, SQLite, output, diagnostics, environment variables, or the Ticket Workspace, and it does not scan every persistent artifact or track every intermediate token refresh generation. Existing redaction remains best effort, not a security boundary. This explicit tradeoff avoids credential-containment machinery that the operator does not require; the separate GitHub Write Gateway boundary remains unchanged.

If a Worker leaves the Session cache missing, malformed, or unreadable, the Control Plane durably marks the Run failed and sends the ticket to Needs Attention. Diagnostic evidence is optional: a confirmed minimal report is recorded when the filesystem accepts it, but evidence creation failure cannot leave the Run active or block recovery.

This deliberately gives the trusted Ticket Agent access to the model credential it needs to operate. Protecting the credential from arbitrary code already executing as that trusted Worker is outside the accepted single-user threat model. GitHub write credentials remain excluded from Workers and fenced through the GitHub Write Gateway; this decision does not weaken that separate boundary.

`workflow doctor` copies the same source into a temporary state directory and uses the pinned Worker image to create and resume a real Codex session. A missing, malformed, non-ChatGPT, expired, or rejected cache fails the production contract. The temporary cache is removed after the probe. This supersedes version-only Worker Codex validation as sufficient evidence of readiness.

API-key authentication remains deliberately excluded: it would use separate Platform billing and is not the operator-selected identity for this workflow. Reconsider this decision if the Control Plane becomes multi-user, Workers become untrusted, or concurrent ChatGPT cache refresh proves unreliable.
