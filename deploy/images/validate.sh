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
	# The Tart plugin ships darwin binaries only: on any other OS the macOS
	# template is checked for syntax, where its plugin is not needed.
	if [ "$dir" = macos ] && [ "$(uname -s)" != Darwin ]; then
		packer validate -syntax-only .
		continue
	fi
	packer init .
	case $dir in
	windows)
		set -- -var version=0.0.0 -var "worker_binary=$tmp/forecrew-worker" -var "iso_path=$tmp/windows.iso" \
			-var iso_checksum=none -var "chocolatey_installer_sha256=$(printf '%064d' 0)" -var "output_dir=$tmp/out"
		packer validate "$@" -var license_confirmed=true .
		# The licence check refuses to run without the confirmation, and for that reason.
		if out=$(packer validate "$@" . 2>&1); then
			echo "validate.sh: the Windows template ran without license_confirmed" >&2
			exit 1
		fi
		case $out in
		*license_confirmed*) ;;
		*)
			echo "validate.sh: the Windows template failed, but not on the licence check:" >&2
			echo "$out" >&2
			exit 1
			;;
		esac
		;;
	macos)
		packer validate -var version=0.0.0 -var "worker_binary=$tmp/forecrew-worker" -var "output_dir=$tmp/out" \
			-var "base_vm=ghcr.io/example/macos@sha256:$(printf '%064d' 0)" .
		;;
	*)
		packer validate -var version=0.0.0 -var "worker_binary=$tmp/forecrew-worker" -var "output_dir=$tmp/out" .
		;;
	esac
done
