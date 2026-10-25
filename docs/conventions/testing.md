# Testing

hangar's `docs/conventions/testing.md` holds here: its tools, its examples
and its rules on waiting (`docs/decisions/testing-strategy.md`). This file
says how tower's own runs differ from hangar's.

## Run in parallel

Every top-level test in `test/e2e` and `internal/apiserver` calls
`t.Parallel()` first, and so does a new one. A test that reads what the
whole process shares (counts its goroutines, sets an environment variable,
installs a global hook) stays serial and says why in a line. Each test
builds its own world, so a test that needs `t.Setenv`, a fixed port or a
package variable it writes is one to rework, not to make serial. A test
that fails only in parallel is fixed at its root: its wait is bounded on
the progress it waits for, never retried or given a longer timeout.

`make e2e` (which `make check` runs) runs `test/e2e` without the race
detector, in parallel; `make test-e2e-race` runs it under the detector one
test at a time, as CI's `e2e-race` job does. hangar split it so because
parallel e2e worlds under the detector starve one another. So no job looks
for a race between two e2e worlds: that they share no state rests on the
rule above.

## Run tests while you work

The suites take every CPU there is, so two side by side only slow each
other down, and the load makes tests that are fine fail:
- Run one suite at a time, never `make check` beside `make e2e` or another
  `go test`.
- While iterating, run the package you changed, or one test of it, without
  the race detector: `go test ./internal/apiserver/ -run '^TestABoxComesUp$'`.
  `make e2e` runs the e2e tests alone.
- Run `make check` once before a commit. It skips `make test-e2e-race`,
  which CI runs; run it yourself after changing how `test/e2e`, or what it
  drives, synchronizes.

## The race detector

Every race run leaves the SQLite driver (`modernc.org/sqlite`, SQLite
translated to Go) uninstrumented, through the Makefile's `RACE`: the
detector's checks of its memory accesses took a third of a run's CPU. A
race inside the driver goes unseen; our code's use of it does not.
