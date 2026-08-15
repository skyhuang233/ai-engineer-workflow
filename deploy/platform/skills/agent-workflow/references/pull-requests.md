# Pull Requests and Delivery

Each Executable Ticket owns one persistent delivery branch, one persistent pull request, one accountable Ticket Agent session, and one Delivery Cycle. Successive revisions stay on that chain.

## Inspect

Read the ticket, accepted revision, pull-request head/base, required checks, reviews, review threads, and current Plan/Inbox state. A new head, relevant base advance, unresolved Review Feedback, or stale Run Lease invalidates earlier readiness.

## Respond to Review Feedback

Route configured-owner reviews, inline comments, and pull-request conversation to the same Ticket Agent. The agent must answer with the change or rationale, commit, and test evidence, then run the complete delivery cycle again. Never mark a human review thread resolved; resolution belongs to the human reviewer.

## Deliver

The Delivery Controller may validate, update, push through the fenced Gateway, publish, and monitor the pull request. Required checks must pass against the current revision and integration base. Optional checks remain diagnostic unless repository policy makes them required.

Only an authorized human may merge or enqueue a delivery pull request. After merge, consider the ticket Delivered only when the accepted revision is reachable from the target branch. A closed-unmerged pull request freezes the Plan for an impact decision; do not open a competing delivery chain.
