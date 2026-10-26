#!/usr/bin/env bash
# base-image.sh NAME makes the base image NAME a VM boots
# (VMSpec.BaseImage): DIR/NAME.raw, a raw disk image the VM driver writes
# onto a blank disk at its first boot. DIR is $TOWER_BASE_IMAGES, or
# /var/lib/tower-hostd/base-images, where hostd's --base-images points.
#
#   ubuntu-24.04  Ubuntu's cloud image, pinned in versions.env, with
#                 ansible-core installed: a machine's golden phase is an
#                 ansible playbook run in the VM (`ansible-playbook
#                 --connection=local`), so the base must carry ansible
#                 before the playbook can run.
#
# Idempotent: an image made already is left as it is; delete it to make
# it again. Run as root.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=deploy/hostd/versions.env
. "$here/versions.env"

fail() { echo "FAIL: $*" >&2; exit 1; }
[ "$(id -u)" = 0 ] || fail "must run as root"
[ $# = 1 ] || fail "usage: base-image.sh NAME"
name=$1
dir=${TOWER_BASE_IMAGES:-/var/lib/tower-hostd/base-images}
out=$dir/$name.raw
[ -e "$out" ] && { echo "$out is made already"; exit 0; }
mkdir -p "$dir"
chmod 0755 "$dir"

work=$(mktemp -d "$dir/.making-$name.XXXXXX")
mnt=$work/root
loop=
cleanup() {
	for m in boot/efi boot dev/pts dev proc sys run; do
		mountpoint -q "$mnt/$m" 2>/dev/null && umount -l "$mnt/$m"
	done
	mountpoint -q "$mnt" 2>/dev/null && umount -l "$mnt"
	[ -n "$loop" ] && losetup -d "$loop"
	rm -rf "$work"
}
trap cleanup EXIT

# fetch_image URL SHA256 fetches a cloud image into the work directory.
fetch_image() {
	curl -fsSL --retry 3 -o "$work/image" "$1"
	echo "$2  $work/image" | sha256sum -c --status || fail "$1 does not match its sha256"
	qemu-img convert -O raw "$work/image" "$work/image.raw"
	rm -f "$work/image"
}

# in_root runs a command chrooted into the image's root filesystem, with
# the host's resolver, so its package manager reaches its mirrors.
in_root() {
	chroot "$mnt" /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin DEBIAN_FRONTEND=noninteractive "$@"
}

mount_root() {
	loop=$(losetup --find --show --partscan "$work/image.raw")
	udevadm settle
	mkdir -p "$mnt"
	mount "${loop}p1" "$mnt"
	[ -b "${loop}p16" ] && mount "${loop}p16" "$mnt/boot"
	for m in dev dev/pts proc sys run; do
		mount --bind "/$m" "$mnt/$m"
	done
	# The image's resolv.conf is a link into /run, to systemd-resolved's
	# stub: the chroot uses the host's resolver through the bound /run
	# where the host runs systemd-resolved (Ubuntu). Where it does not
	# (Debian), the link leads nowhere, and the host's resolv.conf stands
	# in for it while the image is made.
	if [ ! -e "$mnt/etc/resolv.conf" ]; then
		mv "$mnt/etc/resolv.conf" "$mnt/etc/resolv.conf.tower-orig"
		cp /etc/resolv.conf "$mnt/etc/resolv.conf"
	fi
}

# restore_resolv puts the image's own resolv.conf back.
restore_resolv() {
	if [ -e "$mnt/etc/resolv.conf.tower-orig" ] || [ -L "$mnt/etc/resolv.conf.tower-orig" ]; then
		rm -f "$mnt/etc/resolv.conf"
		mv "$mnt/etc/resolv.conf.tower-orig" "$mnt/etc/resolv.conf"
	fi
}

case "$name" in
ubuntu-24.04)
	fetch_image "$TOWER_UBUNTU_2404_URL" "$TOWER_UBUNTU_2404_SHA256"
	mount_root
	# Fails rather than going on with stale lists: an install from them
	# says only that the package is not there.
	in_root apt-get update -q --error-on=any
	in_root apt-get install -qy --no-install-recommends ansible-core
	in_root apt-get clean
	rm -rf "$mnt/var/lib/apt/lists/"*
	restore_resolv
	;;
*)
	fail "no base image $name"
	;;
esac

cleanup_mounts() {
	for m in run sys proc dev/pts dev boot; do
		mountpoint -q "$mnt/$m" && umount "$mnt/$m"
	done
	umount "$mnt"
	losetup -d "$loop"
	loop=
}
cleanup_mounts
mv "$work/image.raw" "$out"
chmod 0644 "$out"
echo "made $out"
