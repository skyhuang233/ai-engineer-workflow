# Keep one delivery chain per Executable Ticket

Each Executable Ticket owns one persistent delivery branch and one pull request, while successive Worker Runs are serialized attempts against that same delivery chain. Planning issues cannot be dispatched, and work too large for one independently reviewable pull request must be split before admission. Reusing the branch and pull request makes crash recovery, review revisions, quality-gate retries, and audit history idempotent at the cost of forbidding competing implementations within one ticket.
