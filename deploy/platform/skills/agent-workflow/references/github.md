# Deterministic Managed GitHub Operations

## Credential and repository fence

Run every operation from the admitted checkout through `workflow github`; never use an independently authenticated `gh` process. Start with `workflow github identity --repo (Get-Location).Path` and use its verified PAT login/id/type plus canonical repository/owner to identify historical objects. The command opens Workflow Home read-only, derives the canonical repository from `origin`, requires an eligible Repository Admission, checks the plaintext PAT fingerprint, live-verifies identity and scopes, and verifies owner-guarded repository access. It never prints or passes the PAT in argv. A personal repository uses the approved `repo,workflow` contract. Organization ownership remains fail-closed with `requires an approved organization scope contract` until the user approves an additional scope contract; do not silently request `admin:org` or any other scope.

Pass `--repo (Get-Location).Path` on every command. List and relation commands fetch every API page. Before each write, read the exact target; after it, read back the exact fields. Reuse an exact match. Stop on zero/multiple candidates, partial state, wrong owner, or a conflicting graph.

## Plan Root, tickets, and labels

Find exact issues across open and closed history; do not limit the search to the first 100:

```powershell
workflow github issue-list --repo (Get-Location).Path --state all
workflow github issue-get --repo (Get-Location).Path --number <issue-number>
```

Match title and body byte-for-byte, the verified Workflow Home credential login with GitHub type `User`, and the single workflow label. A Plan Root has `workflow:plan`; an Executable Ticket has `workflow:ticket`. Create only after all-page readback proves no exact or conflicting candidate:

```powershell
workflow github issue-create --repo (Get-Location).Path --title <title> --body-file <file> --label workflow:plan
workflow github issue-create --repo (Get-Location).Path --title <title> --body-file <file> --label workflow:ticket
```

Use the ticket's numeric database `id` returned by `issue-get`, not its issue number, for native graph writes. For each Plan Root -> ticket edge and each `blocked ticket <- blocker` edge:

```powershell
workflow github subissues-add --repo (Get-Location).Path --number <plan-root-number> --related <ticket-database-id>
workflow github subissues-list --repo (Get-Location).Path --number <plan-root-number>
workflow github blocked-by-add --repo (Get-Location).Path --number <blocked-ticket-number> --related <blocker-database-id>
workflow github blocked-by-list --repo (Get-Location).Path --number <blocked-ticket-number>
```

HTTP 422, a missing relationship, or any edge outside the approved graph leaves publication incomplete. Only after complete graph readback, activate with `workflow github issue-label --repo (Get-Location).Path --number <plan-root-number> --label workflow:active`, then re-read the root and every relationship. The bundle automatically invokes `workflow runtime-configure --source (Get-Location).Path --root <plan-root-issue-number>` after exact activation readback; this is not a user setup step.

## Workflow Inbox

Read all open Inbox candidates and require exactly one issue managed by the verified credential login with GitHub type `User`:

```powershell
workflow github issue-list --repo (Get-Location).Path --state open --label workflow:inbox
workflow github issue-comments --repo (Get-Location).Path --number <inbox-number>
```

After the human chooses one advertised answer, use the Control Plane's atomic question transition exactly once, then read the GitHub projection again:

```powershell
workflow answer-inbox --repository <canonical-owner/repository> --question <question-id> --answer <allowed-answer>
workflow github issue-comments --repo (Get-Location).Path --number <inbox-number>
workflow github issue-get --repo (Get-Location).Path --number <inbox-number>
```

The recognizable projection marker is the exact `# Workflow Inbox` heading. A pending question is the exact Markdown bullet ``- `<question-id>`:``. Poll `workflow github issue-get` until the body still has that heading but no longer has that exact bullet. Only that transition acknowledges the local answer and queued projection; comments alone never do. Never synthesize, edit, or move an uncertain answer.

## Pull requests and review handoff

Resolve the ticket branch's single pull request and exact current revision with managed, fully paginated reads:

```powershell
workflow github pr-list --repo (Get-Location).Path --state all --head <branch>
workflow github pr-get --repo (Get-Location).Path --number <pull-request-number>
workflow github pr-comments --repo (Get-Location).Path --number <pull-request-number>
workflow github pr-reviews --repo (Get-Location).Path --number <pull-request-number>
workflow github commit-checks --repo (Get-Location).Path --commit <head-sha>
```

Hand owner-authored review comments and inline threads to the same Ticket Agent with pull request number, head SHA, comment/review id, path, line, and body. Re-read head and required checks after every revision. Merge and merge-queue submission are human-only; the managed command intentionally exposes no merge operation.
