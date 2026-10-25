# Testing

hangar's `docs/conventions/testing.md` holds here: its tools, its examples
and its rules on waiting (`docs/decisions/testing-strategy.md`). This file
says how tower's own runs differ from hangar's.

## The race detector

Every race run leaves the SQLite driver (`modernc.org/sqlite`, SQLite
translated to Go) uninstrumented, through the Makefile's `RACE`: the
detector's checks of its memory accesses took a third of a run's CPU. A
race inside the driver goes unseen; our code's use of it does not.
