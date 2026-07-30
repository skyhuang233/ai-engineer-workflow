# Reserve review-thread resolution for humans

The Ticket Agent must answer each Review Feedback thread with the change or rationale, relevant commit, and test evidence, then rerun the full Delivery Cycle and request review of the new revision. It cannot mark a human review thread resolved; only a human reviewer decides that the feedback has been satisfied, and unresolved-conversation branch protection may continue blocking merge. Agent and automation replies are excluded from feedback routing so this evidence loop cannot wake itself.
