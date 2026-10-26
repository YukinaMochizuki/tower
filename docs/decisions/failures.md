# What failed waits out a backoff, and says so

Status: accepted, 2026-10; a retry asked for, 2026-10-26.

## Context

tower does work on a target for its objects: it builds goldens, and on
the way to a box or machine being ready it runs steps in its instance: a
box's secret files and checkouts (with its devcontainer's setup), a
machine's configure phase and ready check. Such work fails: a Dockerfile
that does not build, a playbook that exits 2, a checkout of a ref that is
not there. A failure tried again at once, on every reconcile, is the same
failure over and over, a load on the target, and an object whose status
flips between FAILED and starting, which workstream cannot act on.

But a failure often has a cause outside the work, fixed outside tower: on
the first pilot, a registry's request timeout, and build steps without
DNS on the host. Once each was fixed, the backoff kept answering the old
failure for hours, read each time as a new one, and the only way past it
was deleting tower's records by hand.

## Decision

- **A failure waits out a backoff before the work runs again:** 10 minutes,
  doubled after each failure in a row, up to 6 hours. Meanwhile its golden,
  box or machine is FAILED, and its condition says why, when it failed
  ("failed at") and when it runs again ("built again at", "tried again
  at"): a failure read later is seen to be the old one, not a new one.
- **A retry a person asks for runs the work at once.** An administrator
  (`tower retry golden|box|machine NAME -p PROJECT`), or workstream for
  the person it acts for, calls RetryService
  (`proto/tower/environments/v1alpha1/retry_service.proto`): the failures
  counted against the work are forgotten, so it runs now, and a failure
  again waits out the first backoff. Of a box or a machine, its golden
  is built again if that failed, and its failed steps run again. It is a
  call, not a field of the spec: it asks for an act once and leaves
  nothing to undo, and a golden, which tower's controllers alone write,
  has no spec a client may change. The automatic backoff is unchanged:
  tower retries early only when asked.
- **The failures are counted where a restart of tower keeps them**: in
  internal records of the store (`GoldenBackoff` per project and inputs,
  `StepBackoff` per box or machine and step), which no client reads.
- **A success forgets them; so does a fresh start.** A golden's are of its
  inputs, so other inputs start afresh. A step's are of one run of its
  instance and one generation of its box or machine: the instance started
  again, or a change of the spec, runs the step at once.
- **A target that does not answer is no failure.** The work waits for it,
  and runs once it answers.
- **What uses a FAILED box is refused at once, with the reason**: a
  workstream call into it, or a wake. A machine is used only when READY.
- What the work is FAILED for is said without secrets: a step given a
  secret as its environment fails with its exit code alone.

## Consequences

- A fix outside tower (a repository pushed, a registry back) is picked up
  when the backoff runs out, or at once by a retry or by changing the
  object's spec. tower does not notice the fix by itself.
- RetryService is open to ROLE_ADMIN, ROLE_WORKSTREAM and root on the
  host, not to hostd: what runs again is tower's decision
  (`docs/decisions/layers.md`), made for a person.
- A failed step's record goes with its box or machine.
