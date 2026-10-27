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
- **On a hostd host, the target that built an image uses it at once,
  from its own Docker; the push to the registry runs in the background.** (2026-10, after a
  cold box on a target behind a ~14 Mbit/s uplink spent ~8.5 of its ~10
  minutes sending a 1.6 GB image to the registry and pulling it straight
  back.) On a hostd host BuildKit exports the build to an OCI layout
  hostd keeps until the image is pushed; hostd loads that layout into
  Docker, tagged `<repository>:sha256-<digest>` because Docker's own image
  store cannot name an image it loaded by a manifest digest, and makes
  containers of it from there, with no pull. A build FROM an image not
  pushed yet reads it from its layout (a BuildKit named context), and
  resolving one reads its config there too. The push sends the layout
  byte for byte, so the registry holds the very digest the target runs,
  tagged with it so the registry's collection of untagged manifests never
  takes it. Other targets still pull from the registry.
- **On a hangar target, buildkitd pushes each build to the registry as it
  exports it, and the build answers once it has.** Its builder, the
  system container (`internal/system`), has no Docker to run an image
  from, and each of its calls is a process of its own, so it keeps
  nothing between them. The push goes through the container's tailscale
  proxy, as buildkitd's pulls do. Each build is pushed under a tag of its
  own (`build-<random>`): pushed untagged, it would take `latest` from
  the build before it, and the registry's collection of untagged
  manifests would take that one while a golden still names it. What goes
  from the registry stays tower's to decide.
- tower also decides what is pruned from the registry.

Not a public registry: these images are gigabytes each, built often, and
carry toolchain configuration that has no reason to leave the tailnet.

**A hangar host reaches the registry without joining the tailnet** (a
public cloud joins no private network; decided 2026-10). The registry's
read side is open to the hangar host's public IP through an IP allowlist,
and hangar pulls with a read-only login of the registry's that is the
hangar target's own (`HangarTarget.registry_pull_secret`, kept in hangar as
a registry-auth Secret its containers name): every guest of the host leaves
through the same public IP, so the address alone says nothing of who
pulls. Pushing from a hangar target is its builder's: its system
container (`internal/system`), whose buildkitd pushes through the
tailscaled beside it, joined to the tailnet once with an auth key of the
target's (`HangarTarget.tailnet_auth_secret`), an identity the tailnet's
ACL limits to the registry; the host never joins. It pushes to the
registry's tailnet name (`--registry`), and tower names what it built by
the name hangar pulls (`HangarTarget.registry_host`): one registry, two
names.

## Consequences

- Providers need two primitives for images, build-and-push and run-from-the-
  registry, and policy does not branch. On hostd "push" means "pushed
  eventually": a build returns once the image is on the target.
- **A push that fails is tried again, after a wait that doubles up to five
  minutes, until it is done**, a hostd restarted taking up what the last
  left (the layouts are on disk). It never fails the golden: the image is
  fine where it runs. hostd reports it on its Target as the condition
  `ImagesPushed` (False, `Pushing` or `PushFailed` with the images left
  and the last error, until all are pushed) and logs each failure.
- **hostd's sweep never removes an image not pushed yet**, whatever names
  it, and the layout it pushes from is no image Docker prunes.
- Nothing on the building target waits on a push, so tower's controllers
  wait on none: a box image golden is per target and its image is used
  only there. Should an image built on one target ever be run on another,
  the controller that places it must wait for that target's
  `ImagesPushed` to say it is in the registry; tower does not do that yet,
  as nothing asks it to.
- Until an image is pushed the host holds it twice: in Docker, and as the
  layout it is pushed from (both counted in `usage.images`).
- The registry is one more service to run and back up.
- **A build step runs on a network of its own.** A Dockerfile's `RUN` is a
  project's code, run by BuildKit on the host. BuildKit's OCI worker runs
  it in the host's network namespace unless told otherwise; on a hostd
  host it is told `networkMode = "cni"`: each step has a namespace of its
  own on the bridge `tower-build0` (no workstream's; `TOWER_BUILD_SUBNET`,
  10.231.0.0/24 by default), masqueraded out. The nftables table
  `tower_build` (`deploy/hostd/build-network.nft`, loaded as buildkitd
  starts) leaves it the internet and DNS: it drops what the bridge sends
  to the host, at any of its addresses, and what it sends to Docker's
  bridges (every workstream's network, a VM's container's among them),
  the tailnet, and any private, shared, link-local or multicast address
  but on port 53. Pulling a `FROM` and pushing the result are BuildKit's
  own, in the host's namespace, as they were. Two steps building at once
  share the bridge, and reach each other: nothing filters a bridge's own
  traffic without `br_netfilter`. The testbed's
  `TestABuildStepReachesNothingOfTheHost` holds this.
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
