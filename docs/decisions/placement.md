# One target per project

Status: accepted, 2026-10.

## Context

A target is where environments run: a hangar endpoint and project, or a host
running hostd. The boxes and machines of one workstream must reach each other
by name on any port (a box runs kubectl against a k3s machine), which
nothing provides across targets. Goldens, git mirrors and caches live on a
target, so environments spread over targets share none of them.

## Decision

- **All of a project's environments run on one target.** The hard constraint
  is one target per workstream; one per project (one workstream product) is
  chosen so a product's goldens and caches are shared by all its
  workstreams.
- **tower holds the targets, their credentials and which project runs where,**
  set by an administrator with tower's CLI. workstream's intents name only
  the project; workstream never learns target names. It shows the location
  tower reports.
- **Placement is a fact an administrator sets, not a choice tower makes at
  runtime.** tower never spills a project onto another target because the
  first is full.
- Moving a project to another target affects only environments made after
  the move; existing ones stay where they are until they are reclaimed or
  deleted.

## Consequences

- A project moved to another target starts cold there: its goldens and
  caches are rebuilt on the new target.
- One target's outage stops every environment of the projects placed on it.
