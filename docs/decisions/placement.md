# One target per project

Status: accepted, 2026-10. Revised 2026-10-28: a person in workstream
picks a project's target.

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
- **tower holds the targets, their credentials and which project runs where.**
  An administrator registers targets with tower's CLI. Which target a
  project runs on is picked by a person: in workstream's product settings,
  which lists the Targets and applies the Placement named by the project,
  or by an administrator with the CLI. workstream's environment intents
  still name only the project. `ROLE_WORKSTREAM` may apply a Placement but
  not delete one, which would leave the project's environments UNPLACED;
  that stays an administrator's.
- **Placement is a fact a person sets, not a choice tower makes at
  runtime.** tower never spills a project onto another target because the
  first is full.
- Moving a project to another target affects only environments made after
  the move; existing ones stay where they are until they are reclaimed or
  deleted. workstream does not leave them there: it moves a project only
  once none of its boxes and machines is left, deleting them when the
  person asks it to, so a workstream's environments never span targets.
- **A target is deleted only once nothing tower made on it is left.** A
  Target can be deleted once no Placement or Instance names it. Its
  finalizer, `tower.hangarlab.cloud/teardown`, holds it, being deleted,
  while tower deletes its Goldens (and through them their artifacts) and
  then, on a hangar target, every object its provider made in the hangar
  project, the system container's included. A hangar project is shared
  and has a quota, and what tower leaves there counts against it, unseen
  by anyone.
- **An administrator may abandon a target whose teardown cannot finish**
  (`TargetActionService.AbandonTarget`), such as one whose hangar refuses
  its token, or a host gone for good. tower never gives up on its own: it
  cannot tell a target gone for good from one down for a while. What the
  teardown last found left is said, and logged, as not confirmed gone.

## Consequences

- A project moved to another target starts cold there: its goldens and
  caches are rebuilt on the new target.
- One target's outage stops every environment of the projects placed on it.
- A target that does not answer cannot be deleted without an administrator:
  it stays, being deleted, its status saying what is left and why, until
  it answers again or is abandoned (`docs/runbook/deploy.md`).
