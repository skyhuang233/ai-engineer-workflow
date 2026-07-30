# Fence repeated Worker Runs instead of promising exactly-once execution

Worker Runs use at-least-once execution because a failure between local completion and result receipt cannot be distinguished reliably from incomplete work. Every attempt receives a monotonically ordered identity and a time-bounded Run Lease; the Control Plane atomically accepts a Candidate Revision only if that lease is still current. An expired run may overlap physically with its replacement or report late, but its output cannot become the accepted delivery result and remains available only as diagnostic evidence.
