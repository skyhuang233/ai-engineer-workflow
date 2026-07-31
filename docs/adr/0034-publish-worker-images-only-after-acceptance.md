---
status: accepted
---

# Publish Worker images only after acceptance

Pull-request validation may build and test a proposed Worker environment but receives no package-publishing authority and creates no Worker Image Release. Only after the owner accepts the change into `main` may a GitHub Actions job use its short-lived repository `GITHUB_TOKEN` to build from that accepted commit, publish the image to GHCR, and report the immutable registry digest. This prevents rejected candidate code from writing to the formal Worker supply chain while keeping package credentials out of the Control Plane and Worker containers.
