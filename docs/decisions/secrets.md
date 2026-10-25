# Every product secret is tower's

Status: accepted, 2026-10.

## Context

Environments need credentials: a privileged box carries a profile's cloud or
forge credentials, a blueprint asks for secrets at provision time, a target
pulls private images. workstream kept them in a vault of its own and injected
them into a box's environment or a playbook's variables. With the
environments run by tower, either every intent carries its secrets, or tower
keeps them; but tower also rebuilds goldens and re-materializes reclaimed
boxes in the background, where no intent is in flight.

## Decision

- **tower holds every product secret** as a project-scoped `Secret`, after
  hangar's design (`hangar: docs/decisions/secrets.md`): values are
  write-only, never in any answer, event, log or error; stored sealed under a
  data key of their own, wrapped by a key-encryption key in a file outside the
  database; status shows only keys, whether they are set, and a keyed HMAC.
  workstream remains where a human sets them, writing to tower's API.
- **A secret goes only to the environment that uses it, never into a spec
  of a lower layer.** On hangar a Container's spec is stored in plain text in
  hangar's store, so tower injects a value per command, or writes it through
  a command into a file in memory in the box. On hostd that is a tmpfs
  every box container has at `/run/tower/secrets`, written at each start;
  the path a secret file asks for is a link to it, so no disk of the host
  holds a value. Machines take no secret files: a VM would need the same
  in memory first.
- **Goldens never see secrets.** A blueprint's secrets are given only in each
  machine's own configure phase.
- **Privileged boxes run through tower like any box.** Which command may run
  on one is still judged in workstream: every command reaches the box through
  tower, after workstream's judge has passed it.
- workstream keeps only the credentials it runs on: the forge token for pull
  requests, and its agents' model credentials.

## Consequences

- tower is where a leak of product credentials would come from, and is held
  to that.
- Secrets reach the target they are used on, hangar or hostd.
