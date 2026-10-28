# Service tokens with one role each, on the tailnet

Status: accepted, 2026-10.

## Context

tower's callers are few and are programs: workstream, one hostd per host, and
an administrator's CLI. tower holds product secrets and write credentials to
forges, and two things it serves must be workstream's alone: pushing a branch
and running a command on a privileged box. So a caller's identity has to say
which of them it is, not only that it is on the tailnet.

## Decision

- **Every caller sends a service token** (`twr_` and 43 random characters)
  as a bearer token. tower keeps only its SHA-256, after hangar's design
  (`hangar: docs/decisions/authentication.md`): looked up by its hash and
  compared in constant time; shown once, when it is created or rotated;
  revoked the moment it is rotated or its token is deleted; and refused if
  its token was deleted and made again under the same name.
- **A token has exactly one role, fixed when it is made:**

  | Role | May |
  |---|---|
  | `ROLE_ADMIN` | manage tower: targets, projects, tokens, secrets |
  | `ROLE_WORKSTREAM` | state intents in every project and drive their environments, including push and commands on a privileged box; pick a project's target (apply, not delete, a Placement) |
  | `ROLE_HOSTD` | read the objects of its one target and report their status |

  A token that needs another role is another token.
- **Root on tower's host** reaches tower through a unix socket, identified by
  its peer credentials, with an administrator's powers. That is where the
  first token is made.
- **tower listens on the tailnet alone,** which is a second fence, not the
  identity.

Not tailscale's identity: it names a machine, so every program on workstream's
host would be workstream.

## Consequences

- A role's permissions are a table in `internal/authz`; a new kind adds its
  rows there in the change that adds it.
