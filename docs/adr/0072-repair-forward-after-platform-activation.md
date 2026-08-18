---
status: accepted
---

# Repair forward after platform activation

Activation writes the new Generation and Attempt as `repair_required`; only
successful live readiness yields `ready`. Pre-activation failure leaves the old
generation active. Post-activation failure remains visibly repairable through
the same Bundle and Attempt: no automatic pointer rollback exists.
