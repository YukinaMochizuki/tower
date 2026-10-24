# Architecture

The target system. Parts that do not exist yet are described as they are
planned; the code is the authority wherever it exists.

## Shape

```
  workstream (director, store, server)
     │  intents (apply + watch), exec / files / push      tailnet
     ▼                                                       │
  tower-apiserver ── SQLite ── controllers (all policy)      │
     │                              │                        │
     │ hangar API (client)          │ objects hostd watches  │
     ▼                              ▼                        │
  hangar A, hangar B             hostd on host C ◀───────────┘
  (Containers, VMs,              (docker, qemu, ZFS;
   Disks, CustomImages)           dials tower, keeps no state)
                    \            /
                     registry (shared, beside tower)
```

## Layers

| Layer | Decides | Does |
|---|---|---|
| workstream | what is wanted: a box of repo X at ref Y with these add-ons, a machine of this blueprint, keep this box | applies intents, reads status, runs commands through tower |
| tower | everything else: where a project's environments live, when a golden is rebuilt, what is cached, what is reclaimed and when, what is pre-warmed | reconciles intents into resources on a target |
| hangar, hostd | nothing | create, delete, exec, connect, snapshot, clone, report |

`docs/decisions/layers.md` says why policy lives in tower alone.

## Components

| Component | Runs as | Responsibility |
|---|---|---|
| `tower-apiserver` | unprivileged user, on a machine of its own on the tailnet | The API (Connect), authentication by service token, authorization by role, the object store, and every controller |
| `hostd` | root, on each non-hangar host | Turns the objects tower places on its host into containers, VMs and ZFS datasets; reports their state; never decides anything |
| registry | beside tower | Every image a target runs, built on the target and pushed here |
| `tower` | the user | CLI |

## Resource model

Every resource has hangar's envelope: `api_version`, `kind`, `metadata`,
`spec`, `status`. Clients own `spec`; controllers own `status`. Apply replaces
the whole `spec`. The engine is hangar's, copied
(`docs/decisions/engine-from-hangar.md`).

A `Project` is one workstream product, named by the product's id, and the
scope every per-product object lives in; deleting it deletes its contents.
A `ServiceToken` is a client's credential with one role
(`docs/decisions/authentication.md`). A `Secret` holds a project's
credentials, write-only and sealed at rest (`docs/decisions/secrets.md`).

The kinds that carry environments (targets, boxes, machines, goldens,
repositories) are being designed.

## Data flow

1. workstream applies an intent; tower authenticates, authorizes, admits and
   stores it.
2. tower's controllers reconcile it into resources on the project's target:
   hangar objects through hangar's API, or objects hostd watches.
3. hostd converges its host and reports status; tower reports status on the
   intent.
4. Commands and file reads go workstream -> tower -> target; long-lived
   streams (an editor, a forwarded port, ssh) go to the target directly on a
   ticket tower issues (`docs/decisions/traffic.md`).

## Where the reasoning lives

`docs/decisions/` records why each of these choices was made.
