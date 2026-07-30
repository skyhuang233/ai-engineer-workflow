# Reserve final merge authority for humans

The Delivery Controller may validate, publish, update, and monitor a pull request but cannot decide or execute its final merge. Only an authorized human reviewer may merge or enqueue a Merge-Ready revision; a new head, relevant base-branch change, or new Review Feedback invalidates that state and requires renewed validation. GitHub Merge Queue is an optional optimization where the repository plan supports it, while strict branch protection and current-head checks remain the portable baseline.
