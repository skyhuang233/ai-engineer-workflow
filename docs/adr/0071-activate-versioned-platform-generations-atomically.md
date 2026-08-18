---
status: accepted
---

# Activate versioned Platform Generations atomically

Bundle-digest-named generations have immutable payload and generation-local
databases. Atomic `platform/active.json` is the sole activation authority: it
first names candidate and Attempt as `repair_required`, then becomes `ready`
only after live verification. Previous generations remain frozen for diagnosis.
