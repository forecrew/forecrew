#!/bin/sh
# Runs `packer validate` on every image template with placeholder inputs, so a
# broken template is found without licensed hosts or a build. Needs packer
# (make images-validate; CI installs it).
set -eu

command -v packer >/dev/null || {
	echo "validate.sh: packer is not installed" >&2
	exit 1
}
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
: >"$tmp/forecrew-worker"
: >"$tmp/windows.iso"

for dir in linux windows macos; do
	echo "== $dir"
	cd "$here/$dir"
	packer init .
	case $dir in
	windows)
		packer validate -var version=0.0.0 -var "worker_binary=$tmp/forecrew-worker" -var "iso_path=$tmp/windows.iso" \
			-var iso_checksum=none -var license_confirmed=true -var "output_dir=$tmp/out" .
		# The licence check refuses to run without the confirmation.
		if packer validate -var version=0.0.0 -var "worker_binary=$tmp/forecrew-worker" -var "iso_path=$tmp/windows.iso" \
			-var iso_checksum=none -var "output_dir=$tmp/out" . >/dev/null 2>&1; then
			echo "validate.sh: the Windows template ran without license_confirmed" >&2
			exit 1
		fi
		;;
	*)
		packer validate -var version=0.0.0 -var "worker_binary=$tmp/forecrew-worker" -var "output_dir=$tmp/out" .
		;;
	esac
done
