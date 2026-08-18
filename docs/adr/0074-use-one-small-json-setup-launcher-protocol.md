---
status: accepted
---

# Use one small JSON Setup Launcher Protocol

The Bootstrap Skill invokes verified Launcher `inspect`, `apply`, and `verify`
through one schema-versioned UTF-8 JSON stdin/stdout document. `inspect`
distinguishes active-work preflight from target state. Requests reject unknown
fields before mutation; responses may grow diagnostic extensions. Apply carries
target and consent, never a Platform Bootstrap Plan or digest.
