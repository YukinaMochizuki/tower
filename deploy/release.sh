#!/usr/bin/env bash
# release.sh DIR builds a release of the checked-out commit into DIR/dist
# (docs/runbook/release.md):
#
#   tower-host_TAG_linux_amd64.tar.gz   what a host installs from: bin/
#                                       (tower-apiserver, tower, hostd,
#                                       tower-guest), deploy/, the deploy
#                                       runbook and VERSION
#   tower_TAG_OS_ARCH.tar.gz            the CLI, for Linux and macOS on
#                                       amd64 and arm64
#   tower-system_TAG_linux_amd64.tar    a hangar target's system image, an
#                                       OCI layout (deploy/system-image)
#   VERSION                             the commit
#   SHA256SUMS                          every other file's sha256
#
# TOWER_RELEASE_TAG names the release. Every binary is static
# (CGO_ENABLED=0: the store is modernc.org/sqlite, pure Go), so one build
# runs on Ubuntu and Debian alike. Run from the repository's root.
set -euo pipefail

fail() { echo "FAIL: $*" >&2; exit 1; }
[ $# = 1 ] || fail "usage: release.sh DIR"
: "${TOWER_RELEASE_TAG:?the tag being released}"
tag=$TOWER_RELEASE_TAG
out=$1
commit=$(git rev-parse HEAD)
[ -z "$(git status --porcelain)" ] || fail "the working tree is not clean"

rm -rf "$out"
mkdir -p "$out/dist"
export CGO_ENABLED=0
build() { # build GOOS GOARCH DIR PACKAGE...
	local os=$1 arch=$2 dir=$3
	shift 3
	GOOS=$os GOARCH=$arch go build -trimpath -o "$dir/" "$@"
}

host=$out/host
mkdir -p "$host/bin" "$host/docs/runbook"
build linux amd64 "$host/bin" ./cmd/tower-apiserver ./cmd/tower ./cmd/hostd ./cmd/tower-guest
cp -r deploy "$host/deploy"
rm -f "$host/deploy/release.sh"
cp docs/runbook/deploy.md "$host/docs/runbook/"
echo "$commit" >"$host/VERSION"
# Owned by root and dated by the commit, so the same commit packs the same.
when=$(git show -s --format=%cI HEAD)
tar_dir() { # tar_dir FILE DIR
	tar --sort=name --owner=0 --group=0 --numeric-owner --mtime="$when" \
		-C "$2" -czf "$1" .
}
tar_dir "$out/dist/tower-host_${tag}_linux_amd64.tar.gz" "$host"

for os in linux darwin; do
	for arch in amd64 arm64; do
		cli=$out/cli-$os-$arch
		mkdir -p "$cli"
		build "$os" "$arch" "$cli" ./cmd/tower
		tar_dir "$out/dist/tower_${tag}_${os}_${arch}.tar.gz" "$cli"
	done
done

deploy/system-image/build.sh "$out/dist/tower-system_${tag}_linux_amd64.tar"

echo "$commit" >"$out/dist/VERSION"
(cd "$out/dist" && sha256sum -- * >SHA256SUMS)
echo "release $tag of $commit in $out/dist"
