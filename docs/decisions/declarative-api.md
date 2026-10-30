# A declarative API: intents outlive what materializes them

Status: accepted, 2026-10.

## Context

workstream's box and machine interfaces are imperative: create, get, stop,
destroy. tower could serve the same calls and reconcile behind them, or serve
resources that clients apply and watch, as hangar does.

What decided it is that tower acts on boxes by itself: it stops a box that has
been idle and starts it again when it is used, and is meant later to release
the disk of boxes under pressure and bring them back. With imperative calls,
tower would stop or delete something workstream believes it holds, and would
need a protocol to tell workstream, which would then have to remember what it
wanted: a status, reinvented. Opening an environment also takes minutes (an image build, a
clone, a seed), and an imperative call interrupted halfway needs its own
operation records to finish.

## Decision

- **Clients apply intents as resources and watch their status.** A `Box`
  object is what workstream wants; its status says whether it is
  materialized, and where. A box tower stopped for idleness keeps its object,
  with a status that says so, and runs again on its next use: the identity
  workstream holds stays valid throughout. One whose node has no room to
  start it stays asked to run and starts by itself once there is room; the
  call that woke it is told so after a short wait, not held to its deadline
  (`boost.md`, "A full node"). A box whose disk is released will
  keep it the same way, once tower does that.
- **Policy is controllers.** Rebuilding a golden, stopping idle boxes and
  pre-warming are reconcilers over specs and statuses, as everything else
  is, and are tested the same way.
- **Imperative work hangs off a resource as RPCs:** running a command,
  reading and writing files, copying between environments, pushing a branch,
  issuing a stream ticket.
- workstream's client wraps "apply, then wait until ready or failed", so a
  director's tool call stays one synchronous call.

## Consequences

- A failure shows up as a condition on the status, which workstream turns
  into an error a director can read; spec errors are still refused at apply.
- tower needs a resource engine; it takes hangar's
  (`engine-from-hangar.md`).
