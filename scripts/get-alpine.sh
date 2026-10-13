#!/bin/sh
# Downloads the latest Alpine minirootfs, checks its SHA-256 against Alpine's
# release index and unpacks it as the "alpine" image.
set -eu

dest=${1:-/var/lib/microbox/images/alpine}
arch=$(uname -m) # x86_64, aarch64, ...
base="https://dl-cdn.alpinelinux.org/alpine/latest-stable/releases/$arch"

if [ "$(id -u)" -ne 0 ]; then
	echo "run as root: files in the rootfs must stay owned by root" >&2
	exit 1
fi
if [ -e "$dest" ]; then
	echo "$dest already exists; remove it to download again"
	exit 0
fi

# latest-releases.yaml is a list of entries starting with "-"; take file and
# sha256 from the minirootfs one.
entry=$(curl -fsSL "$base/latest-releases.yaml" | awk '
	/^-/ { mini = 0; file = ""; sum = "" }
	/^  flavor: alpine-minirootfs$/ { mini = 1 }
	/^  file:/ { file = $2 }
	/^  sha256:/ { sum = $2 }
	mini && file != "" && sum != "" { print file, sum; exit }
')
if [ -z "$entry" ]; then
	echo "no minirootfs entry in $base/latest-releases.yaml" >&2
	exit 1
fi
file=${entry% *}
sum=${entry#* }

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
curl -fsSL -o "$tmp/$file" "$base/$file"
echo "$sum  $tmp/$file" | sha256sum -c - >/dev/null

mkdir -p "$tmp/rootfs"
tar -xzf "$tmp/$file" -C "$tmp/rootfs" --numeric-owner
mkdir -p "$(dirname "$dest")"
mv "$tmp/rootfs" "$dest"
echo "unpacked $file to $dest"
