# tower and the registry run on a machine of their own

Status: accepted, 2026-10.

## Context

tower manages hangar endpoints and hostd hosts; if it ran on one of them, that
target's outage would stop every other target's environments too. It is also
on the path of every command a director runs (`traffic.md`).

## Decision

- **tower and the shared registry run on a dedicated machine, served over
  the tailnet.** hangar is to join the tailnet too, which is how its targets
  reach the registry and how workstream reaches a hangar target's streams.

## Consequences

- That machine's uptime and its latency to workstream sit under every command
  an agent runs.
