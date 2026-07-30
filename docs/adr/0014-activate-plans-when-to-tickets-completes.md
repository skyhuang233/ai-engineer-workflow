# Activate Delivery Plans when to-tickets completes

The user's final approval of the `to-tickets` breakdown authorizes both publication and execution, so no second activation prompt is required. `to-tickets` remains solely responsible for publishing every ticket and establishing native sub-issue and blocking relationships; only its explicit, fully successful completion event moves the Plan Root into active execution. Partial publication or relationship failure leaves the plan inactive and must be retried idempotently before any frontier ticket can be dispatched.
