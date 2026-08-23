# Agent Workflow development policy

## Git Flow

`main` and `develop` are the only long-lived branches. `main` contains
production-ready source; `develop` integrates the next release and is the
repository default branch.

Use only these short-lived branch types:

- `feature/<feature-name>` starts from `develop` and returns to `develop`.
  `<feature-name>` is a lowercase kebab-case capability name, never a person or
  agent name. Ordinary next-release defect work uses this lifecycle too.
- `release-<version>` starts from `develop`. It may contain only the version
  assignment, release metadata or documentation, and release-blocking fixes.
  Merge it separately to `main` and `develop`.
- `hotfix-<version>` starts from `main` for an urgent production repair. Merge
  it to `main` and, when no release branch is active, to `develop`. When a
  `release-*` branch is active, the hotfix must merge to `main` and that active
  release branch; if integration cannot wait, it may additionally merge
  directly to `develop`. Never roll a target branch's version backward.

Every integration uses a pull request and a merge commit equivalent to
`git merge --no-ff`. Do not squash, rebase, or push development commits
directly to `main` or `develop`. Only the human repository owner authorizes a
merge, after applicable CI succeeds. Delete a short-lived branch manually only
after every required target contains it; automatic branch deletion stays off.

This private GitHub Free repository cannot technically enforce all of these
rules with branch protection. They are mandatory process constraints.

## Workflow Releases

`config/workflow-release.json` is the sole product-version source. Assign the
next version only when creating a `release-*` or `hotfix-*` branch; `develop`
otherwise retains the last released version. The unified publisher alone may
create immutable `workflow-v<version>` tags and Releases. Humans and agents
must never create, move, replace, or overwrite them manually.

The first future Release is `workflow-v0.0.1`. The repository is currently in
an intentional release blackout; do not publish it until its release branch is
ready and the owner merges it to `main`.

## Agent skills

### Issue tracker

Issues and PRDs are tracked in this repository's GitHub Issues.

### Domain docs

Single-context layout: root `CONTEXT.md` and `docs/adr/`.
