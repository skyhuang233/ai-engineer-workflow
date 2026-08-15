# Workflow Inbox

## Read

Find the single open repository issue labelled `workflow:inbox`. Read its current body and owner-authored comments. Treat each stable question id independently; links and evidence identify the owning Plan, ticket, pull request, commit, finding, or recovery operation.

Do not treat pull-request comments, Plan Root comments, or free-form conversation as Workflow Inbox answers. Do not accept answers from bots or identities other than the configured owner.

## Answer

Explain the decision, impact, and allowed answers to the user without choosing for them. When the user decides, add exactly one owner-authored line:

```text
workflow-answer:<question-id>: <allowed answer>
```

Preserve structured JSON exactly when the question requires it. Do not reuse an answer for another id, omit the id, or invent an allowed answer. Read back the comment and wait for the Control Plane projection to acknowledge it before claiming the question resumed.

If delivery of the Inbox projection is uncertain, report that uncertainty and follow the projected recovery instruction. Never duplicate the question on another surface or guess whether an unobserved write succeeded.
