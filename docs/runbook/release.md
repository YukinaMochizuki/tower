# Releasing tower, and installing a release on a host

A release is what a host installs from: built by CI from a tag, attested,
and published as the tag's GitHub Release (`.github/workflows/release.yml`,
which runs `deploy/release.sh`). Nothing on a host builds tower.

## Cutting one

```sh
git tag v0.1.0 && git push origin v0.1.0
```

The workflow builds, attests and publishes. Its summary, and the release
notes, carry the digest of `SHA256SUMS`: the one value a host checks before
it trusts anything else in the release. A tag is released once; to publish
it again, delete the release first.

| File | Holds |
|---|---|
| `tower-host_TAG_linux_amd64.tar.gz` | `bin/` (`tower-apiserver`, `tower`, `hostd`, `tower-guest`), `deploy/`, `docs/runbook/deploy.md`, `VERSION` |
| `tower_TAG_OS_ARCH.tar.gz` | the CLI, Linux and macOS, amd64 and arm64 |
| `SHA256SUMS`, `VERSION` | every file's sha256; the commit |
| `attestation.jsonl` | build provenance of every file |

Every binary is static, so one host tar serves Ubuntu and Debian.

## Installing one on a host

As root, with the tag and the digest from the release notes:

```sh
TAG=v0.1.0 DIGEST=<sha256 of SHA256SUMS>
url=https://github.com/YukinaMochizuki/tower/releases/download/$TAG
dir=/usr/local/lib/tower/releases/$TAG
install -d "$dir" && cd "$dir"
curl -fsSLO "$url/SHA256SUMS" && echo "$DIGEST  SHA256SUMS" | sha256sum -c
curl -fsSLO "$url/tower-host_${TAG}_linux_amd64.tar.gz"
sha256sum -c --ignore-missing SHA256SUMS
tar -xzf "tower-host_${TAG}_linux_amd64.tar.gz"
```

`$dir` then holds what `docs/runbook/deploy.md` installs from: run its steps
from `$dir`, where it says `bin/` and `deploy/`. The repository is private,
so the downloads need a token that reads it (`curl -H "Authorization: Bearer
..."`, or `gh release download $TAG -R YukinaMochizuki/tower`, then the same
checks).

An upgrade is the same download into a directory of its own, then
`docs/runbook/deploy.md`'s "Upgrading".
