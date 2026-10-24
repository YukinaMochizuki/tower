# The demo: tower and hostd on a fake host

What `make e2e` runs (`test/e2e`), by hand: a box made on a hostd target,
used, idle-stopped and woken, and its project deleted. hostd runs on a fake
host (the `hostdfake` build tag), so nothing on the machine is touched; the
fake host is kept in `fake-host.json` in hostd's state directory.

Three terminals, from the repository root.

## 1. tower

```sh
make dev
```

`make dev` keeps its database, admin socket, key-encryption key and ticket
key under `.cache/dev/` (`docs/runbook/dev-env.md`); the ticket key's
public half is `.cache/dev/ticket-key.pub`.

## 2. Tokens, the target and a box

```sh
go build -o .cache/dev/tower ./cmd/tower
alias tower=.cache/dev/tower

# The first admin token, as root on tower's host (the admin socket).
TOWER_SERVER=unix://.cache/dev/admin.sock tower token create admin --role admin --secret-file .cache/dev/admin.token
export TOWER_SERVER=http://127.0.0.1:8080 TOWER_TOKEN_FILE=.cache/dev/admin.token

tower token create hostd-main --role hostd --target main --secret-file .cache/dev/hostd.token
tower token create workstream --role workstream --secret-file .cache/dev/workstream.token

tower apply -p web -f - <<'EOF'
apiVersion: tenancy.tower.hangarlab.cloud/v1alpha1
kind: Project
metadata: {name: web}
---
apiVersion: targets.tower.hangarlab.cloud/v1alpha1
kind: Target
metadata: {name: main}
spec:
  hostd: {}
  settings: {idleStopAfter: 30s}
---
apiVersion: targets.tower.hangarlab.cloud/v1alpha1
kind: Placement
metadata: {name: web}
spec: {target: main}
---
apiVersion: environments.tower.hangarlab.cloud/v1alpha1
kind: Repository
metadata: {name: api}
spec: {url: "https://git.example.com/api.git", defaultBranch: main}
---
apiVersion: environments.tower.hangarlab.cloud/v1alpha1
kind: Box
metadata: {name: b1}
spec:
  workstream: ws1
  checkouts: [{repository: api}]
EOF
```

The box waits as `PENDING` until its target has a host: `tower get box -p web`.

## 3. hostd on a fake host

```sh
go run -tags hostdfake ./cmd/hostd --tower http://127.0.0.1:8080 --target main \
  --token-file .cache/dev/hostd.token --ticket-public-key .cache/dev/ticket-key.pub \
  --state-dir .cache/dev/host --direct-listen 127.0.0.1:7443
```

## 4. Watch it, and use it

```sh
tower wait box b1 -p web --for condition=Ready
tower get box b1 -p web -o yaml
tower get target main -o yaml      # Reachable, Reporting, capacity, facts
tower get instances -p web         # what hostd made, and its phase
```

A command, and a ticket for a direct stream, as workstream asks for them
through EnvironmentService (`Exec` is a server stream, which `buf curl`,
pinned in `tools/tools.mod`, speaks):

```sh
buf() { go tool -modfile=tools/tools.mod buf "$@"; }
auth="Authorization: Bearer $(cat .cache/dev/workstream.token)"
buf curl --schema proto --protocol connect -H "$auth" \
  -d '{"environment":{"project":"web","box":"b1"},"argv":{"args":["echo","hello"]}}' \
  http://127.0.0.1:8080/tower.environments.v1alpha1.EnvironmentService/Exec
buf curl --schema proto --protocol connect -H "$auth" \
  -d '{"environment":{"project":"web","box":"b1"},"port":8080}' \
  http://127.0.0.1:8080/tower.environments.v1alpha1.EnvironmentService/IssueTicket
```

The ticket's `url` is on hostd's `--direct-listen`; a client upgrades an
HTTP/1.1 request there to `tower-stream/1` with the ticket as its bearer
token (`hostd.DialStream`, which `test/e2e` uses), and the fake guest echoes
what it is sent.

A process in the box, which runs apart from any call and keeps the box
from being stopped for idleness while it does (the fake's `serve` writes
its arguments, then runs until it is signalled):

```sh
box='"environment":{"project":"web","box":"b1"}'
buf curl --schema proto --protocol connect -H "$auth" \
  -d "{$box,\"argv\":{\"args\":[\"serve\",\"listening\"]},\"name\":\"server\"}" \
  http://127.0.0.1:8080/tower.environments.v1alpha1.EnvironmentService/StartProcess
# its id is "process" in the answer, such as p1
buf curl --schema proto --protocol connect -H "$auth" -d "{$box,\"process\":\"p1\",\"follow\":true}" \
  http://127.0.0.1:8080/tower.environments.v1alpha1.EnvironmentService/AttachProcess
buf curl --schema proto --protocol connect -H "$auth" -d "{$box,\"process\":\"p1\",\"signal\":\"SIGNAL_TERM\"}" \
  http://127.0.0.1:8080/tower.environments.v1alpha1.EnvironmentService/SignalProcess
buf curl --schema proto --protocol connect -H "$auth" -d "{$box}" \
  http://127.0.0.1:8080/tower.environments.v1alpha1.EnvironmentService/ListProcesses
```

`tower get instances -p web -o yaml` shows `processesRunning` while it runs.

After `idleStopAfter` with no command and no process running, plus up to a minute, the box is
`STOPPED`; the next command starts it again, with the same id. Stopping
hostd (Ctrl-C) makes the target `Reachable=False` and commands
`Unavailable`; starting it again finds the host as it was.

## 5. Delete it

```sh
tower delete project web
cat .cache/dev/host/fake-host.json   # Disks, Goldens, Guests and Networks empty once it settles
```

Mirrors and pulled images stay: they are the host's caches, which no
Instance or Golden accounts for.
