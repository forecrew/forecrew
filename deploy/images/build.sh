#!/bin/sh
# Builds a Forecrew Linux image and writes its manifest next to it.
#
#   build.sh qcow2 --version 2026.11.1 --worker ./forecrew-worker --uri-base https://images.example.com [--key image.key]
#   build.sh rootfs --version 2026.11.1 --worker ./forecrew-worker --uri-base https://images.example.com \
#       --kernel ./vmlinux [--key image.key]
#
# The output is in deploy/images/out. Without --key the manifest is unsigned:
# a person signs it with `forecrew image sign` before `forecrew image add`.
# Needs packer, qemu (qcow2) or docker (rootfs), and the forecrew CLI (FORECREW,
# default `forecrew` on PATH). The rootfs also needs mke2fs 1.47.1 or newer with
# libarchive (the package libarchive13t64 on Debian and Ubuntu); `forecrew image
# rootfs` says if yours cannot. On a host with an older one set
# MKE2FS=deploy/images/mke2fs-docker.sh. ARCH is amd64 (default) or arm64.
set -eu

die() {
	echo "build.sh: $*" >&2
	exit 1
}

[ $# -ge 1 ] || die "usage: build.sh qcow2|rootfs --version V --worker PATH --uri-base URL [--kernel FILE] [--key FILE]"
kind=$1
shift
version="" worker="" uri_base="" kernel="" key="" arch=${ARCH:-amd64}
while [ $# -gt 0 ]; do
	[ $# -ge 2 ] || die "$1 needs a value"
	case $1 in
	--version) version=$2 ;;
	--worker) worker=$2 ;;
	--uri-base) uri_base=$2 ;;
	--kernel) kernel=$2 ;;
	--key) key=$2 ;;
	*) die "unknown option $1" ;;
	esac
	shift 2
done
[ -n "$version" ] && [ -n "$worker" ] && [ -n "$uri_base" ] || die "--version, --worker and --uri-base are required"
[ -f "$worker" ] || die "$worker is not a file"
case $kind in qcow2 | rootfs) ;; *) die "kind is qcow2 or rootfs" ;; esac
[ "$kind" != rootfs ] || [ -n "$kernel" ] || die "a rootfs image needs --kernel, the kernel the microVM boots"

# abspath prints an absolute path: the build changes directory, so every path
# and command the caller gave must not depend on where it started.
abspath() {
	case $1 in
	/*) echo "$1" ;;
	*/*) echo "$(cd "$(dirname "$1")" && pwd)/$(basename "$1")" ;;
	*) command -v "$1" || die "$1 is not on PATH" ;;
	esac
}
forecrew=$(abspath "${FORECREW:-forecrew}")
worker=$(abspath "$worker")
[ -z "$kernel" ] || kernel=$(abspath "$kernel")
[ -z "$key" ] || key=$(abspath "$key")
[ -z "${MKE2FS:-}" ] || MKE2FS=$(abspath "$MKE2FS")
here=$(cd "$(dirname "$0")" && pwd)
out=$here/out
mkdir -p "$out"

(
	cd "$here/linux"
	packer init .
	only=linux.qemu.qcow2
	[ "$kind" = qcow2 ] || only=linux.docker.rootfs
	packer build -only="$only" -var "version=$version" -var "arch=$arch" -var "worker_binary=$worker" -var "output_dir=$out" .
)

# manifest FORMAT FILE URI PACKAGES-FILE [more flags]: writes <FILE>.manifest.json,
# with a --package for every name=version line of the build's list.
manifest() {
	m_format=$1 m_file=$2 m_uri=$3 m_packages=$4
	shift 4
	while IFS= read -r line; do
		[ -z "$line" ] || set -- "$@" --package "$line"
	done <"$m_packages"
	[ -z "$key" ] || set -- "$@" --key "$key"
	"$forecrew" image manifest --name forecrew-linux --version "$version" --os linux --arch "$arch" --format "$m_format" \
		--file "$m_file" --uri "$m_uri" --built-at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --out "$m_file.manifest.json" "$@"
}

name=forecrew-linux-$version-$arch
common="--label go --label node --label python --label git --label runtime:claude-code --label runtime:codex --label runtime:opencode"
if [ "$kind" = qcow2 ]; then
	# shellcheck disable=SC2086
	manifest qcow2 "$out/linux-qcow2-$arch/$name.qcow2" "$uri_base/$name.qcow2" "$out/packages-qcow2-$arch.txt" $common --label docker
else
	cd "$here"
	# shellcheck disable=SC2086
	"$forecrew" image rootfs --tar "$out/linux-rootfs-$arch.tar" --name "$name" --worker "$worker" --cache "$out/cache" ${MKE2FS:+--mke2fs "$MKE2FS"} $common >"$out/rootfs.path"
	cp "$(cat "$out/rootfs.path")" "$out/$name.ext4"
	# shellcheck disable=SC2086
	manifest rootfs "$out/$name.ext4" "$uri_base/$name.ext4" "$out/packages-rootfs-$arch.txt" \
		--kernel "$kernel" --kernel-uri "$uri_base/$(basename "$kernel")" $common
fi
echo "build.sh: wrote $out/$name.*.manifest.json"
