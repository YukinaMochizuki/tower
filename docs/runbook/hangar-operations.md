# Operating hangar targets

What `docs/runbook/deploy.md` ("5. A hangar target") leaves to the
operator: the registry's public, read-only side that hangar pulls from;
the hangar tenant tower runs in; adding a product; rotating credentials;
sizing a Plan; remaking a target's system container; and what to look at
when something is wrong. Names in angle brackets are the operator's own;
no address, host or account below is a real one.

The shape, one hangar project and one Target per product:

```
tower's machine                         hangar host
  tower-apiserver ── API (https) ──────▶ hangar-edge ─▶ project <product>
  zot :5000 (tailnet)                      ├ tower-sys-*  (mirrors, builds)
  nginx :5443 (public, GET/HEAD) ◀── pull ─┤ boxes, VMs
        ▲                                   │
        └── DNAT, from the hangar host's addresses only
```

## The registry's public side

hangar pulls every box image and the system image by a name the hangar
host reaches, and the tailnet is not one of them
(`docs/decisions/images.md`). So the registry gets a second entry: public,
TLS, read-only, open to the hangar host's addresses alone, with a login of
its own. Pushes stay on the tailnet.

**1. A name.** An A record `<registry name>` (such as
`registry.example.com`) for the public address in front of tower's
machine. If that address is a proxy's (a CDN), turn the proxy off: the
entry is TLS end to end, on a port of its own.

**2. A forward, from the hangar host alone.** When tower's machine is
behind NAT, forward the port from the public address on the router or
hypervisor in front of it, one rule for each of the hangar host's source
addresses, and nothing from anywhere else:

```sh
iptables -t nat -A PREROUTING -i <public interface> -p tcp --dport 5443 \
  -s <hangar host address> -j DNAT --to-destination <tower machine's private address>:5443
```

If the router's FORWARD policy drops by default, accept the same
traffic there too. Persist the rules where the router keeps its own (on a Debian/Proxmox
host, a `post-up` line on the bridge in `/etc/network/interfaces`), and
keep a copy of the file before you change it.

**3. A certificate.** lego with a DNS-01 challenge, so nothing has to be
open on port 80. The DNS provider's token is the machine's alone:

```sh
install -d -m 0700 /etc/tower/acme
install -m 0600 /dev/stdin /etc/tower/acme/<dns provider>-token   # paste the token
CLOUDFLARE_DNS_API_TOKEN_FILE=/etc/tower/acme/<dns provider>-token \
  lego --path /etc/tower/acme/lego --email <operator email> \
  --dns cloudflare --domains <registry name> --accept-tos run
```

A daily systemd timer (`tower-registry-cert.timer` and its service) runs
the same command with `renew` in place of `run`, and reloads nginx when a
certificate was renewed, through lego's renew hook (its flag's name is
lego's version's: see `lego renew --help`). Check it with `systemctl
list-timers` and `lego --path /etc/tower/acme/lego list`.

**4. nginx, GET and HEAD only.** It listens on the machine's private
address, the forward's destination, and proxies to zot on the tailnet,
verifying zot's tailnet certificate:

```nginx
server {
    listen <tower machine's private address>:5443 ssl;
    server_name <registry name>;
    ssl_certificate     /etc/tower/acme/lego/certificates/<registry name>.crt;
    ssl_certificate_key /etc/tower/acme/lego/certificates/<registry name>.key;

    client_max_body_size 0;
    proxy_read_timeout 1h;
    proxy_buffering off;

    location /v2/ {
        limit_except GET { deny all; }          # GET also allows HEAD
        proxy_pass https://<tower's tailnet name>:5000;
        proxy_ssl_server_name on;
        proxy_ssl_name <tower's tailnet name>;
        proxy_ssl_verify on;
        proxy_ssl_trusted_certificate /etc/ssl/certs/ca-certificates.crt;
        proxy_ssl_verify_depth 5;               # the tailnet certificate's chain is longer than nginx's default
        proxy_set_header Host $host;
        proxy_set_header Authorization $http_authorization;
    }
    location / { return 404; }
}
```

**5. zot's users and who may do what.** install.sh makes zot with one
htpasswd user, `hostd`. Add two more, each with a password of its own in
`/root/registry-<user>.password`:

| User | May | Used by |
|---|---|---|
| `hostd` | read, write | every hostd host |
| `tower-builder` | read, write | each hangar target's system container (its builds' pushes), and the operator's push of the system image |
| `hangar-pull` | read | hangar, through the public side |

and give zot an `accessControl` that says so (back up
`/etc/zot/config.json` first; install.sh, run again, writes the file
without it):

```json
"accessControl": {
  "repositories": {
    "**": {
      "policies": [
        { "users": ["hostd", "tower-builder"], "actions": ["read", "create", "update", "delete"] },
        { "users": ["hangar-pull"], "actions": ["read"] }
      ]
    }
  }
}
```

under `http`, beside `auth`; then `systemctl restart zot`.

**Check it.** From the hangar host:

```sh
curl -sS -o /dev/null -w '%{http_code}\n' https://<registry name>:5443/v2/   # 401, and no certificate error
```

From anywhere else the connection is never answered. With
`hangar-pull`'s password, a pull works and a push is refused.

## The hangar side

tower is one tenant of the hangar, on a Plan of its own, with one hangar
project per tower product (`docs/decisions/placement.md`: one target per
project). As hangar's platform admin:

1. A Plan for tower; sizing is below. Its `projects` is how many products
   tower may place there.
2. A tenant on that Plan.
3. For each product, a project in that tenant, and a service token of
   `project.editor` on it, for a year. The token is shown once; it goes
   straight into tower's Secret (below), and nowhere else.

## Adding a product

The product is switched to tower in workstream first
(`workstream: docs/runbook/tower-pilot.md`); tower then has a project named
by the product's id. As tower's administrator:

1. **A hangar project and its token**, as above.
2. **Its Secret** in the reserved project `system`:
   `hangar-<product>-token`, key `token`:

   ```sh
   tower secret set hangar-<product>-token -p system --from-stdin token   # paste the token
   ```
3. **The shared Secrets**, made once, and used by every hangar target of
   the same hangar and registry:
   - `hangar-registry-pull`: a `registry-auth` for `<registry name>:5443`,
     user `hangar-pull`;
   - `hangar-registry-push`: keys `username` (`tower-builder`) and
     `password`;
   - `hangar-tailnet`: key `authkey` (below).
4. **The Target**:

   ```yaml
   apiVersion: targets.tower.hangarlab.cloud/v1alpha1
   kind: Target
   metadata: {name: hangar-<product>}
   spec:
     hangar:
       endpoint: https://<hangar api>
       project: <hangar project>
       credential_secret: hangar-<product>-token
       registry_pull_secret: hangar-registry-pull
       registry_host: <registry name>:5443
       registry_push_secret: hangar-registry-push
       tailnet_auth_secret: hangar-tailnet
       base_images:
         ubuntu-24.04: ubuntu-24-04
   ```

   `tower get target hangar-<product>` should, within a couple of
   minutes, be Reachable and Reporting, with the facts showing the system
   container Ready, on the tailnet, and `mirror transport: endpoint`.
5. **The Placement**, named by the product's id:

   ```yaml
   apiVersion: targets.tower.hangarlab.cloud/v1alpha1
   kind: Placement
   metadata: {name: <product id>}
   spec: {target: hangar-<product>}
   ```

   Every repository of the product is then fetched into the target's
   mirrors; `tower get repo -p <product id> -o yaml` shows each one's
   mirror.

A Placement is not deleted with its project: deleting a tower project
leaves its Placement, which an administrator deletes by hand.

## The tailnet

Each target's system container joins the tailnet itself, to push to the
registry and to read repositories served on the tailnet. In the
configuration repository's policy (`infra: tailnet/base.hujson`) it is
`tag:tower-system`, with grants to the registry's port on
`tag:tower-control` and to each tailnet git service it reads (workstream's).
Its auth key is a reusable, not ephemeral key, tagged `tag:tower-system`;
the node it makes has key expiry off, as tagged nodes do.

The key is read only when a system container joins: when it is first
made, or remade without its disk (below). A running target does not need
it to be valid.

## Rotating credentials

| Credential | Lifetime | Where it lives | To rotate |
|---|---|---|---|
| hangar project token | 1 year | Secret `hangar-<product>-token` | make a new token on the project, write the Secret, then revoke the old token; the Target picks it up on its next call |
| tailnet auth key | 90 days | Secret `hangar-tailnet` | make a new key with the same tag, write the Secret; only the next join reads it |
| registry users' passwords | none | `/root/registry-<user>.password`, zot's htpasswd, and the Secrets above | `htpasswd -bB /etc/zot/htpasswd <user> <new>`, restart zot, then write `hangar-registry-pull` or `hangar-registry-push` (and each hostd host's, for `hostd`) |
| DNS provider token | the provider's | `/etc/tower/acme/<dns provider>-token` | replace the file; the next renewal uses it |
| the public certificate | 90 days | `/etc/tower/acme/lego` | renewed by the timer; nothing to do while it runs |
| the key-encryption key | none | `/var/lib/tower/secrets-kek`, and the vault | `docs/runbook/deploy.md`, "1. The apiserver" |

## Sizing a Plan

hangar counts a disk at the size it declares, in the tenant's quota and on
the node, however little has been written to it (its volumes are sparse,
but the quota is not). So the Plan is sized from what tower declares:

| What | Disk declared |
|---|---|
| a box | 2Gi for `/workspace` and 2Gi for its container root at first (the root at its image's size when that is larger), each growing as it fills up to 30Gi: `/workspace` by 4Gi once 80% full, the root by 2Gi once 90% full; or as the Target's settings say |
| a warm golden's builder | as a box, but `/workspace` starts a quarter over what the repository's last warm golden held, or as a box's (2Gi) for the first |
| a VM | 20Gi, or its Image's size if larger |
| a target's system container | 4Gi, growing by 4Gi once 80% full up to 50Gi (BuildKit keeps its cache under 20GB), plus a root from 2Gi (or its image's size, if larger) growing by 2Gi once 90% full up to 10Gi; one made before this tower release keeps its disks until it is made again (below) |
| a golden | the size of what it captured, as a CustomImage |

A box's disks grow by themselves (hangar's disk auto-grow): the
`/workspace` disk from `box_workspace_disk` (or the box's own
`resources.disk`, or its warm golden's size, if larger) up to
`box_workspace_disk_max`, the container root from `box_root_disk` (or
its image's size, if larger: hangar starts it there) up to
`box_root_disk_max`, in the Target's `spec.settings`. Each grows by a
fixed step once it is full enough: `/workspace` by 4Gi at 80%, the root
by 2Gi at 90%, as `box_workspace_disk_grow_step` and
`box_workspace_disk_grow_threshold`, and `box_root_disk_grow_step` and
`box_root_disk_grow_threshold`, may say otherwise (a whole percentage
from 50% to 95%; a factor over 1x and at most 16x, or a quantity of at
least 1Gi, as hangar takes them). The root starts at its image's base,
mostly full, so it grows later and by less: doubling it would count the
image twice. A warm golden's builder grows as a box. hangar counts a
grown disk at its grown size, so size the Plan for boxes at their max. A
box made before this tower release keeps the disks it was made with, and
how they grow, until it is made again.

A stopped box keeps its disks, so it counts. Disk is what runs out
first. The other limits a Plan sets, per target: a network per workstream
plus the system container's; a network endpoint per workstream network;
`machine_calls` above the most execs, port forwards and VM connections
open at once (deploy.md, "hangar's machine calls"); `custom_images` for
the goldens; and `projects`, one per product.

## Remaking a target's system container

A hangar Disk only grows. To give a target's system container a smaller
disk (after a release lowers the default), or to start it again
from nothing, delete its container and then its disk in the hangar
project with tower's apiserver stopped; tower makes both again once it
starts, at the current default. Stop it first: it checks the system
container every 30 seconds, and one that finds the container gone makes
it again on the disk still there, which then cannot be deleted. The
stop takes every target's API down for as long as the deletes take,
usually seconds:

```sh
hangar get containers -p <hangar project> -l tower/role=system
sudo systemctl stop tower-apiserver                          # on tower's host
hangar delete container tower-sys-<hash> -p <hangar project>
hangar delete disk tower-sys-<hash> -p <hangar project>     # once the container is gone
sudo systemctl start tower-apiserver
```

Remove the old tailnet node (below) before tower starts again, or the new
one joins as `tower-builder-<target>-1`.

What goes with the disk: BuildKit's cache (the next builds are cold), the
mirrors (fetched again, every repository of the product), and the
container's tailnet state, so it joins again with `hangar-tailnet`'s key,
which must then be valid. The tailnet keeps the old node: remove it from
the admin console. The target is back when its facts show the system
container Ready and on the tailnet. Its repositories still say Fetched,
of the heads the old mirrors had: until each is fetched again, at most
ten minutes later, a box of it fails `DevcontainerUnresolvable` ("the
repository has no <commit>"), and is to be made again after.

## When something is wrong

- **`tower get target <name> -o yaml`** is the first place: its
  conditions (Reachable, Reporting, DiskPressure), `last_error`, its room
  by the quota, and its facts: the system container, the builder's
  BuildKit, `mirror transport` (`endpoint`, or `relay:` and why), `mirror
  transfers`, and the hangar quota's use.
- **The apiserver's log**: `journalctl -u tower-apiserver`. hangar's front
  door refusing tower is logged as `hangar's front door refused requests`
  (deploy.md, "hangar's front door").
- **The system container's own log** is hangar's: `hangar logs container
  tower-sys-<hash> -p <hangar project>`, and `hangar exec container
  tower-sys-<hash> -p <hangar project> -- <command>` to look inside it.
- **A pull hangar cannot make** (an image's machine failing to start) is
  most often the public side: its certificate expired, the forward lost
  after a reboot of the router, or the `hangar-pull` password changed in
  zot but not in `hangar-registry-pull`. Check it from the hangar host as
  above.
- **The registry is never pruned.** Box images and system images build up
  in `/var/lib/zot/storage`; remove the ones no golden names by hand.
