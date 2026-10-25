# One shared registry; images are built on the target

Status: accepted, 2026-10.

## Context

A box runs an image: a repository's devcontainer, with add-ons layered on
it. hangar runs containers only from a registry (with a login per pull,
`hangar: docs/decisions/secrets.md`); a hostd host could keep images in its
own Docker. If each provider stored images its own way, every policy about
images would branch by provider.

## Decision

- **A registry of tower's own (such as zot), beside tower on the tailnet,
  holds every image a target runs.** Every target pulls from it.
- **tower decides when an image is built; the build runs on the project's
  target:** with Docker or BuildKit on a hostd host, with a builder in the
  hangar project on hangar. The result is pushed to the registry.
- tower also decides what is pruned from the registry.

Not a public registry: these images are gigabytes each, built often, and
carry toolchain configuration that has no reason to leave the tailnet.

## Consequences

- Providers need two primitives for images, build-and-push and run-from-the-
  registry, and policy does not branch.
- The registry is one more service to run and back up.
- **Known gap: one project can reach another's images.** Every hostd signs
  in to the registry as one user, which reads and writes every repository,
  and a box may name any image (`BoxSpec.image`, `fallback_image`, a
  devcontainer's `image`, a Dockerfile's `FROM`), so a project's box can be
  made of another project's image, or a build of one can push over
  another's repository. Accepted for the pilot, where one product is on a
  target; it must be closed before a second project shares a target, one
  of two ways:
  - the apiserver limits which of the registry's repositories a project
    may name (its own `<project>/` alone) in what it builds and runs, and
    names nothing else of tower's registry; or
  - each project has registry credentials of its own, scoped to its
    repositories, which hostd uses for that project's pulls and builds
    alone.
