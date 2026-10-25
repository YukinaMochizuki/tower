# What failed waits out a backoff, and says so

Status: accepted, 2026-10.

## Context

tower does work on a target for its objects: it builds goldens, and on
the way to a box or machine being ready it runs steps in its instance: a
box's secret files and checkouts (with its devcontainer's setup), a
machine's configure phase and ready check. Such work fails: a Dockerfile
that does not build, a playbook that exits 2, a checkout of a ref that is
not there. A failure tried again at once, on every reconcile, is the same
failure over and over, a load on the target, and an object whose status
flips between FAILED and starting, which workstream cannot act on.

## Decision

- **A failure waits out a backoff before the work runs again:** 10 minutes,
  doubled after each failure in a row, up to 6 hours. Meanwhile its golden,
  box or machine is FAILED, and its condition says why and when it runs
  again ("built again at", "tried again at").
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
  when the backoff runs out, or at once by changing the object's spec.
- A failed step's record goes with its box or machine.
