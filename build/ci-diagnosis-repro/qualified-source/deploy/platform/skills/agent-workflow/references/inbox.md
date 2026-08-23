# Workflow Inbox

## Read

Find the single open repository issue labelled `workflow:inbox`. Read its current body and owner-authored comments. Treat each stable question id independently; links and evidence identify the owning Plan, ticket, pull request, commit, finding, or recovery operation.

Do not treat pull-request comments, Plan Root comments, or free-form conversation as Workflow Inbox answers. Do not accept answers from bots or identities other than the verified Workflow Home credential login.

## Answer

Explain the decision, impact, and allowed answers to the user without choosing for them. When the user decides, apply the exact allowed answer through the Control Plane's atomic local transition:

```powershell
workflow answer-inbox --repository <canonical-owner/repository> --question <question-id> --answer <allowed-answer>
```

Preserve structured JSON exactly when the question requires it. Do not reuse an answer for another id, omit the id, or invent an allowed answer. `workflow answer-inbox` atomically records local state and queues the projection; never replace it with a GitHub comment write. Poll `workflow github issue-get --repo (Get-Location).Path --number <inbox-number>`. The exact `# Workflow Inbox` heading is the projection marker and ``- `<question-id>`:`` is the pending field. Treat the answer as acknowledged only when the heading remains and that exact pending field is absent. Only then claim the question resumed.

If delivery of the Inbox projection is uncertain, report that uncertainty and follow the projected recovery instruction. Never duplicate the question on another surface or guess whether an unobserved write succeeded.
