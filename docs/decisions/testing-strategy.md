# Testing strategy

Status: accepted, 2026-10.

## Context

One person maintains tower, and coding agents write most of its tests. What
tower drives (hangar, Docker, Cloud Hypervisor, ZFS) is not available in an
ordinary workspace or CI runner. hangar solved the same problem
(`hangar: docs/decisions/testing-strategy.md`), and tower takes its answer.

## Decision

| Layer | What runs | Where | When |
|---|---|---|---|
| L0 | Unit tests | workspace, CI | every PR |
| L1 | Control plane: real apiserver, SQLite, Connect, CLI | workspace, CI | every PR |
| L2 | Hermetic full stack: tower, hostd and the CLI, with fake providers and fake host drivers | workspace, CI | every PR |
| L3 | Conformance: one suite per interface, run against the fake and the real implementation (the provider interface against hangar and hostd; hostd's drivers against the host) | fake: CI; real: a testbed | every PR; real when an implementation changes |

- **Everything outside tower sits behind an interface with a fake** that can
  inject faults: a provider that fails to boot, a full disk, an environment
  that vanished, a target that stops answering.
- **Conformance suites keep fakes honest,** which is what lets L2 be trusted.
- **The control plane injects a clock,** so timeouts, idleness and reclaim
  can be fast-forwarded.
- hangar's rules on flaky tests and on waiting hold unchanged: a flake is
  fixed at its root, never retried; `time.Sleep` is forbidden in tests by
  lint; in-process timing runs in `testing/synctest`; tests that run the
  apiserver wait with `controllertest.WaitSettled`; "nothing happened" is
  checked on the object itself.
- **The stress job** is tower's own `.github/workflows/stress.yml`, which
  sets `TOWER_RUN_QUARANTINED=1`, so a test quarantined with
  `testutil.Quarantine` still runs there. `make stress` (`PKG=`, `COUNT=`)
  puts the same load on one machine, so a flake can be reproduced in a
  workspace: on the branch and on `main`, to tell a change that broke a test
  from a test that was already flaky.

## Consequences

Most of tower is tested end to end without a hangar or a host. Only the real
providers and hostd's real drivers need a testbed.
