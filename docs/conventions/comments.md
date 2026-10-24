# Code comments

A comment says what the code cannot say for itself, and it must still be true
and meaningful to someone who was never part of the conversation that produced
it, a year from now.

## Write

- **Why, not what.** A non-obvious constraint, an invariant, a quirk of an
  external system, the reason an ordering matters:

  ```go
  // CH rejects ondemand restore when any device uses vhost-user, so the root
  // disk stays virtio-blk.
  ```

- **Doc comments on exported identifiers.** One line, starting with the name,
  saying what it is or what it guarantees, not how it works. A method that
  implements a documented interface method needs none of its own:

  ```go
  // Revision is the store-wide write counter; every write increments it by one.
  type Revision int64
  ```

- **A package doc** of one paragraph: what the package is responsible for, and
  what it deliberately is not.

The default is no comment. Good names and small functions come first.

## Do not write

| Kind | Example | Instead |
|---|---|---|
| Essays | A ten-line background explanation | More than three lines belongs in `docs/`. Leave one line and the file path |
| The discussion that led here | `// Option B (instead of A)`, `// we chose X over Y` | Nobody else knows what A and B were. Only when the rejected approach looks obviously right, write one line: `// Not X: <reason>.` |
| References that rot | `// see plan item 3`, `// per M1`, `// see docs §4.2` | Section numbers, list positions and milestone names change. Refer to a file path |
| Time and history | `// new:`, `// now uses`, `// previously`, `// changed to` | That is what git history is for |
| Restating the code | `// increment the counter` | Delete it |
| Conversational voice | `// Note: I added this because...` | State the fact |
| Ownerless TODOs | `// TODO: handle this` | `// TODO(#123): ...`, pointing at an issue |
| Commented-out code | | Delete it; git remembers |

## Where the rest goes

- **Why this change was made** goes in the commit message and the PR
  description.
- **Why the system is shaped this way** goes in `docs/decisions/`.
- **How to do something** goes in `docs/runbook/`.

Reviewers check every new or changed comment against this file; see
`docs/checklists/review.md`.
