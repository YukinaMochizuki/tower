# The resource engine is hangar's, copied

Status: accepted, 2026-10.

## Context

tower needs what hangar already has: resources declared in proto with
generated Connect services, apply semantics, a SQLite store with a global
revision and a resumable watch, controllers with a queue, and a test helper
that waits until every controller has settled. The same person maintains
both. Three ways to share it were weighed: a module of its own, imported by
both; hangar exporting its generic packages; and a copy.

## Decision

- **tower copies the engine from hangar and keeps hangar's approach
  throughout,** so the two can later be extracted into one shared module
  mechanically. The copy was taken at hangar
  `5c1ca49b4615e9ab8be789b3d84f7c73b5e817c4`; the first commit of this
  repository is that copy unchanged, and the next one only rewrites import
  paths, so `diff -r` against hangar shows every later change.
- **Copied:** `internal/apimachinery`, `internal/store` (with its
  conformance suite), `internal/controller` (with `controllertest`),
  `internal/client`, `internal/testutil`, the root of `internal/auth`,
  `internal/auth/token`, `internal/auth/session` cut to service tokens'
  credentials (the name is kept for the extraction), `internal/secrets`,
  `proto/tower/meta/v1`, `tools/protoc-gen-tower`, and the generic parts of
  hangar's apiserver.
- **Not copied:** tenancy and plans, quota, role bindings, people's sign-in
  and browser or CLI sessions, the node tunnel, the edge, public endpoints, monitoring.
  tower's authorization is a role per token (`authentication.md`).
- **These hangar decisions hold for tower unchanged,** read with "tower" for
  "hangar" and `tower.hangarlab.cloud` as the API domain:
  `docs/decisions/apply-semantics.md`, `api-rpc-shape.md`,
  `resource-codegen.md`, `sqlite-store.md`, `buf-breaking-wire-json.md` and
  `api-versioning.md` in hangar. Their tenancy parts do not apply: in tower,
  everything per-product is owned by its `Project`.
- **The engine keeps hangar's word for scope, "project".** A tower `Project`
  is one workstream product.

Not a shared module now: the apiserver's generic code is woven with hangar's
authorization and quota, and both repositories would change across a module
boundary at every step while hangar is still moving fast.

## Consequences

- A fix to the engine is made twice until the extraction. A change to a
  copied package says in its commit whether hangar needs it too.
- tower follows hangar's Go version and pinned tools.
