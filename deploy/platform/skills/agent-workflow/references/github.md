# Deterministic GitHub Operations

## Bind identity and repository

Run all commands inside the admitted checkout. Do not infer identity from Git configuration.

```powershell
gh auth status --active
$actor = gh api user --jq '{login,id,type}' | ConvertFrom-Json
$repo = gh repo view --json nameWithOwner -q .nameWithOwner
$origin = git remote get-url origin
```

Require `$actor.login` to equal the configured human owner identity and `$actor.type` to be `User`. Require `$repo`, `$origin`, the Repository Admission, and the Workflow projection to identify the same repository. Stop on a missing, ambiguous, bot, or mismatched identity. Pass `-R $repo` to every `gh issue` or `gh pr` command and use `repos/$repo/...` for `gh api`; never rely on another current directory.

Before each write, read the target. After it, read back the exact fields below. If the desired state already exists exactly, reuse it. If no state exists, create it once. If multiple candidates or a partial/conflicting state exists, stop and report it instead of duplicating or overwriting it.

## Labels, Plan Root, and tickets

Read labels with `gh label list -R $repo --limit 100 --json name,color,description`. The admitted Repository Contract owns `workflow:plan`, `workflow:ticket`, `workflow:active`, and `workflow:inbox`; do not create substitutes.

Find an exact existing Plan Root or ticket across open and closed issues before creating one:

```powershell
gh issue list -R $repo --state all --limit 100 --json number,title,body,state,labels,author
gh issue create -R $repo --title $title --body-file $bodyFile --label workflow:plan
gh issue create -R $repo --title $title --body-file $bodyFile --label workflow:ticket
gh issue view $number -R $repo --json number,id,title,body,state,labels,author,parent,subIssues,blockedBy
```

Match the approved title and body byte-for-byte and require the expected single workflow label and configured owner author. A Plan Root has `workflow:plan` and never `workflow:ticket`; a ticket has `workflow:ticket` and never `workflow:plan`.

Attach each ticket using immutable numeric database ids, then read the native relationship back:

```powershell
$ticketId = gh api "repos/$repo/issues/$ticket" --jq .id
gh api --method POST "repos/$repo/issues/$root/sub_issues" -F sub_issue_id=$ticketId
gh api "repos/$repo/issues/$root/sub_issues" --paginate --jq '.[] | {id,number,labels:[.labels[].name]}'
```

For every approved `blocked ticket <- blocker` edge:

```powershell
$blockerId = gh api "repos/$repo/issues/$blocker" --jq .id
gh api --method POST "repos/$repo/issues/$blocked/dependencies/blocked_by" -F issue_id=$blockerId
gh api "repos/$repo/issues/$blocked/dependencies/blocked_by" --paginate --jq '.[] | {id,number,state,labels:[.labels[].name]}'
```

HTTP 422, a missing relationship, an unexpected issue, or an edge outside the approved graph leaves publication incomplete. After the complete graph readback, activate with `gh issue edit $root -R $repo --add-label workflow:active`, then verify `gh issue view $root -R $repo --json labels,subIssues` and every dependency endpoint before runtime binding.

## Workflow Inbox

Find the single open Inbox with `gh issue list -R $repo --state open --label workflow:inbox --json number,title,body,author`; zero or multiple results is a blocker. Read owner-authored comments with `gh api "repos/$repo/issues/$inbox/comments" --paginate --jq '.[] | {id,body,user:.user.login,type:.user.type}'`.

After the human chooses one advertised answer, write exactly once:

```powershell
$body = "workflow-answer:$questionId`: $answer"
gh issue comment $inbox -R $repo --body $body
gh api "repos/$repo/issues/$inbox/comments" --paginate --jq '.[] | {id,body,user:.user.login,type:.user.type}'
```

Accept readback only when exactly one configured-owner `User` comment has the exact body. Do not edit, synthesize, or move an answer.

## Pull request and review handoff

Resolve the ticket's single pull request and current revision with:

```powershell
gh pr list -R $repo --state all --head $branch --json number,state,isDraft,headRefName,headRefOid,baseRefName
gh pr view $pr -R $repo --json number,state,isDraft,headRefName,headRefOid,baseRefName,mergeStateStatus,statusCheckRollup,reviews,comments
gh api "repos/$repo/pulls/$pr/comments" --paginate --jq '.[] | {id,body,user:.user.login,path,line,commit_id}'
```

Hand owner-authored review comments and inline threads to the same Ticket Agent with the pull request number, current `headRefOid`, comment or review id, path, line, and body. Re-read the head and checks after every revision. Merge is human-only, including merge-queue submission. The agent may report readiness but must never run `gh pr merge`.
