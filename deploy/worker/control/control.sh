#!/bin/sh
# Entrypoint of the control worker image (docs/runbooks/install.md, Control worker).
#
#   forecrew-control init   register worker "control" in pool "control" and keep
#                           its token in the token volume; does nothing when the
#                           token is already there
#   forecrew-control run    write worker.yaml from the environment and run
#                           forecrew-worker as the forecrew user
#
# "forecrew worker add" shows the token once and cannot be given one, so the
# token cannot come from .env: init stores it in a volume that only this image
# mounts. run reads it as root and drops to forecrew before the worker starts,
# so the agents (which run as forecrew) cannot read it, as install.sh does.
set -eu

name=control
state=/var/lib/forecrew-control
token_file=$state/token
conf=/etc/forecrew-worker/worker.yaml
worker_uid=10001

die() {
	echo "forecrew-control: $*" >&2
	exit 1
}

init() {
	if [ -s "$token_file" ]; then
		echo "forecrew-control: worker $name is registered; keeping its token"
		return 0
	fi
	# Fails when a worker of that name exists without its token in this
	# volume: its token is lost and the worker must be revoked first.
	token="$(forecrew worker add "$name" --pool control)" ||
		die "cannot register worker $name; if it exists and its token is lost, run \"forecrew worker revoke $name\" and start again"
	umask 077
	printf '%s\n' "$token" >"$token_file.tmp"
	mv "$token_file.tmp" "$token_file"
	echo "forecrew-control: registered worker $name in pool control"
}

run() {
	[ -s "$token_file" ] || die "no token in $token_file: run the init step first"
	max_jobs="${FORECREW_CONTROL_MAX_JOBS:-4}"
	case "$max_jobs" in
	'' | *[!0-9]*) die "FORECREW_CONTROL_MAX_JOBS must be a whole number, got \"$max_jobs\"" ;;
	esac
	[ "$max_jobs" -ge 1 ] || die "FORECREW_CONTROL_MAX_JOBS must be at least 1"
	server_url="${FORECREW_CONTROL_SERVER_URL:-http://forecrewd:80}"
	case "$server_url" in
	http://* | https://*) ;;
	*) die "FORECREW_CONTROL_SERVER_URL must start with http:// or https://" ;;
	esac
	case "$server_url" in
	*[!A-Za-z0-9:/._-]*) die "FORECREW_CONTROL_SERVER_URL may only contain letters, digits and : / . _ -" ;;
	esac

	mkdir -p "$(dirname "$conf")"
	cat >"$conf" <<YAML
server_url: $server_url
name: $name
work_dir: /var/lib/forecrew-worker/work
max_jobs: $max_jobs
YAML

	FORECREW_WORKER_TOKEN="$(cat "$token_file")"
	export FORECREW_WORKER_TOKEN
	exec setpriv --reuid="$worker_uid" --regid="$worker_uid" --clear-groups --no-new-privs \
		env HOME=/home/forecrew forecrew-worker run --config "$conf"
}

case "${1:-}" in
init) init ;;
run) run ;;
*) die "usage: forecrew-control init | run" ;;
esac
