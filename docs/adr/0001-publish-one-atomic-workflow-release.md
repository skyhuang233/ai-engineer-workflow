# Publish one atomic Workflow Release

Platform and Worker will share one version and one immutable Workflow Release
instead of publishing independent component releases. This makes compatibility
explicit and gives each Git Flow release branch one unambiguous product outcome,
at the cost of rebuilding and publishing both components whenever either one
changes.
