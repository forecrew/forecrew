#!/bin/sh
# Installs, upgrades or removes forecrew-worker as a service on Linux (systemd)
# or macOS (launchd). Run it as root, with the secrets in the environment: a
# token typed on the command line or in "VAR=value sudo ..." shows up in the
# process list and in the shell history. Read it without echo instead:
#
#   read -rs FORECREW_WORKER_TOKEN && export FORECREW_WORKER_TOKEN
#   sudo --preserve-env=FORECREW_WORKER_TOKEN,GITHUB_TOKEN sh install.sh \
#     --server https://forecrew.example.com --labels go,node
#
# See docs/runbooks/add-worker.md. Run it again to upgrade the binary: the
# existing configuration is kept.
set -eu

usage() {
	cat <<'USAGE'
usage: install.sh [options]
       install.sh --uninstall [--purge]

  --server URL      forecrewd's public URL (required on the first run)
  --token TOKEN     worker token from "forecrew worker add". A flag is visible
                    in the process list: prefer the environment variable
                    FORECREW_WORKER_TOKEN (see the top of this file).
                    Required on the first run; a new one replaces the old one.
  --name NAME       display name (default: the host name)
  --labels a,b,c    toolchains the machine offers, for example go,node,docker
  --work-dir DIR    where job workspaces go (default /var/lib/forecrew-worker/work)
  --max-jobs N      jobs at once (default 1; more only for repos you trust)
  --version TAG     release to install, for example v0.2.0 (default: latest)
  --base-url URL    release location (default https://github.com/forecrew/forecrew/releases)
  --reconfigure     rewrite worker.yaml from these options; otherwise an
                    existing worker.yaml is kept
  --uninstall       stop and remove the service and the binary; keeps the
                    configuration, the token and the work directory
  --purge           with --uninstall: also delete the configuration, the token,
                    /var/lib/forecrew-worker and the service user

Values may contain letters, digits and . _ / : @ + - only.
USAGE
}

die() {
	echo "install.sh: $*" >&2
	exit 1
}

say() { echo "install.sh: $*"; }

# valid_token TOKEN: fcw_ and base64url characters only. The token file is
# read by sh on macOS, so nothing else may reach it.
valid_token() {
	case "$1" in
	fcw_) return 1 ;;
	fcw_*[!A-Za-z0-9_-]*) return 1 ;;
	fcw_*) return 0 ;;
	*) return 1 ;;
	esac
}

server="" token="${FORECREW_WORKER_TOKEN:-}" name="" labels="" work_dir=""
max_jobs="" version="latest" base_url="https://github.com/forecrew/forecrew/releases"
reconfigure=0 uninstall=0 purge=0

while [ $# -gt 0 ]; do
	case "$1" in
	-h | --help) usage; exit 0 ;;
	--uninstall) uninstall=1; shift; continue ;;
	--purge) purge=1; shift; continue ;;
	--reconfigure) reconfigure=1; shift; continue ;;
	--server | --token | --name | --labels | --work-dir | --max-jobs | --version | --base-url)
		[ $# -ge 2 ] || die "$1 needs a value"
		case "$1" in
		--server) server="$2" ;;
		--token) token="$2" ;;
		--name) name="$2" ;;
		--labels) labels="$2" ;;
		--work-dir) work_dir="$2" ;;
		--max-jobs) max_jobs="$2" ;;
		--version) version="$2" ;;
		--base-url) base_url="$2" ;;
		esac
		shift 2
		;;
	*) usage >&2; die "unknown option $1" ;;
	esac
done

# FORECREW_INSTALL_ROOT puts every file under another directory and skips the
# root check. It exists for the tests.
root="${FORECREW_INSTALL_ROOT:-}"
os="${FORECREW_INSTALL_OS:-$(uname -s)}"
if [ -z "$root" ] && [ "$(id -u)" -ne 0 ]; then
	die "run as root (sudo)"
fi

bin_dir="$root/usr/local/bin"
bin="$bin_dir/forecrew-worker"
conf_dir="$root/etc/forecrew-worker"
conf="$conf_dir/worker.yaml"
token_file="$conf_dir/token.env"
state_dir="/var/lib/forecrew-worker"
service="forecrew-worker"
launchd_label="com.forecrew.worker"
case "$os" in
Linux)
	svc_user="forecrew-worker"
	svc_group="forecrew-worker"
	unit="$root/etc/systemd/system/$service.service"
	;;
Darwin)
	svc_user="_forecrew-worker"
	svc_group="_forecrew-worker"
	plist="$root/Library/LaunchDaemons/$launchd_label.plist"
	log_path=/var/log/forecrew-worker.log
	log_file="$root$log_path"
	;;
*) die "unsupported operating system $os (use install.ps1 on Windows)" ;;
esac

# check_value NAME VALUE: values end up in YAML, unit files and shell words.
check_value() {
	case "$2" in
	*[!A-Za-z0-9._/:@+,-]*) die "$1 may contain letters, digits and . _ / : @ + , - only" ;;
	esac
}

# --- uninstall -------------------------------------------------------------

if [ "$uninstall" -eq 1 ]; then
	case "$os" in
	Linux)
		systemctl disable --now "$service" 2>/dev/null || true
		rm -f "$unit"
		systemctl daemon-reload
		;;
	Darwin)
		launchctl bootout "system/$launchd_label" 2>/dev/null || true
		rm -f "$plist"
		;;
	esac
	rm -f "$bin"
	say "removed the $service service and $bin"
	if [ "$purge" -eq 1 ]; then
		rm -rf "$conf_dir" "$root$state_dir"
		rm -f "${log_file:-}"
		case "$os" in
		Linux) userdel "$svc_user" 2>/dev/null || true ;;
		Darwin)
			dscl . -delete "/Users/$svc_user" 2>/dev/null || true
			dscl . -delete "/Groups/$svc_group" 2>/dev/null || true
			;;
		esac
		say "deleted the configuration, the token, $state_dir and the user $svc_user (a custom work directory stays)"
	else
		say "kept $conf_dir and the work directory; add --purge to delete them"
	fi
	exit 0
fi

# --- validate the arguments ------------------------------------------------

[ "$purge" -eq 0 ] || die "--purge only goes with --uninstall"
check_value --server "$server"
check_value --name "$name"
check_value --labels "$labels"
check_value --work-dir "$work_dir"
check_value --max-jobs "$max_jobs"
check_value --version "$version"
check_value --base-url "$base_url"

new_config=1
if [ -f "$conf" ] && [ "$reconfigure" -eq 0 ]; then
	new_config=0
	if [ -n "$server$name$labels$work_dir$max_jobs" ]; then
		say "keeping the existing $conf; add --reconfigure to rewrite it"
	fi
fi
if [ "$new_config" -eq 1 ]; then
	[ -n "$server" ] || die "--server is required"
	case "$server" in
	http://* | https://*) ;;
	*) die "--server must be an http:// or https:// URL" ;;
	esac
	[ -n "$work_dir" ] || work_dir="$state_dir/work"
	case "$work_dir" in
	/*) ;;
	*) die "--work-dir must be an absolute path" ;;
	esac
	if [ -n "$max_jobs" ]; then
		case "$max_jobs" in
		0 | *[!0-9]*) die "--max-jobs must be a positive number" ;;
		esac
		if [ "$max_jobs" -gt 1 ]; then
			say "warning: with --max-jobs above 1 the agents of concurrent jobs can read each other's repo tokens; use it only for repos you trust equally"
		fi
	fi
fi
if [ -n "$token" ]; then
	valid_token "$token" || die "the worker token must be fcw_ followed by letters, digits, - and _ (forecrew worker add prints it)"
elif [ ! -f "$token_file" ]; then
	die "a worker token is required: pass --token or set FORECREW_WORKER_TOKEN"
fi

# --- download ----------------------------------------------------------------

case "${FORECREW_INSTALL_ARCH:-$(uname -m)}" in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) die "unsupported architecture $(uname -m)" ;;
esac
goos=$(echo "$os" | tr '[:upper:]' '[:lower:]')
archive="forecrew-worker_${goos}_${arch}.tar.gz"
if [ "$version" = latest ]; then
	url_dir="$base_url/latest/download"
else
	url_dir="$base_url/download/$version"
fi

tmp=$(mktemp -d)
cleanup() {
	rm -rf "$tmp"
	rm -f "$bin.new" "$conf.tmp" "$token_file.tmp"
}
trap cleanup EXIT INT TERM

# GITHUB_TOKEN switches to the authenticated GitHub API, for private repos. It
# reaches curl through stdin, so it is neither in a process list nor on disk,
# and it is only ever sent to api.github.com.
github_repo=""
case "$base_url" in
https://github.com/*/*/releases) github_repo="${base_url#https://github.com/}" github_repo="${github_repo%/releases}" ;;
esac
use_api=0
if [ -n "${GITHUB_TOKEN:-}" ]; then
	case "$GITHUB_TOKEN" in
	*[!A-Za-z0-9_-]*) die "GITHUB_TOKEN has unexpected characters" ;;
	esac
	if [ -n "$github_repo" ]; then
		use_api=1
	else
		say "ignoring GITHUB_TOKEN: --base-url is not a github.com releases URL"
	fi
fi

api_curl() {
	printf 'header = "Authorization: Bearer %s"\n' "$GITHUB_TOKEN" | curl -fsSL -K - "$@"
}

# fetch NAME: downloads the release asset NAME into $tmp.
fetch() {
	if [ "$use_api" -eq 0 ]; then
		curl -fsSL -o "$tmp/$1" "$url_dir/$1" || die "cannot download $url_dir/$1"
		return
	fi
	asset_url=$(awk -v f="$1" '
		/"url": *"https:\/\/api\.github\.com\/[^"]*\/releases\/assets\/[0-9]+"/ { u = $0; sub(/.*"url": *"/, "", u); sub(/".*/, "", u) }
		$0 ~ "\"name\": *\"" f "\"" { print u; exit }
	' "$tmp/release.json")
	[ -n "$asset_url" ] || die "the release has no asset $1"
	api_curl -H "Accept: application/octet-stream" -o "$tmp/$1" "$asset_url" || die "cannot download $1"
}

say "downloading $archive ($version)"
if [ "$use_api" -eq 1 ]; then
	if [ "$version" = latest ]; then release=latest; else release="tags/$version"; fi
	api_curl -H "Accept: application/vnd.github+json" -o "$tmp/release.json" \
		"https://api.github.com/repos/$github_repo/releases/$release" || die "cannot read the release $version of $github_repo"
fi
fetch "$archive"
fetch checksums.txt
want=$(awk -v f="$archive" '$2 == f { print $1 }' "$tmp/checksums.txt")
[ -n "$want" ] || die "checksums.txt has no entry for $archive"
if command -v sha256sum >/dev/null 2>&1; then
	have=$(sha256sum "$tmp/$archive" | awk '{ print $1 }')
else
	have=$(shasum -a 256 "$tmp/$archive" | awk '{ print $1 }')
fi
[ "$want" = "$have" ] || die "checksum mismatch for $archive"
tar -xzf "$tmp/$archive" -C "$tmp" forecrew-worker || die "cannot unpack $archive"

# --- user, files ---------------------------------------------------------------

# run_as_service COMMAND...: runs a command as the service user, keeping
# FORECREW_WORKER_TOKEN from the environment.
run_as_service() {
	case "$os" in
	Linux) runuser -u "$svc_user" -- "$@" ;;
	Darwin) sudo --preserve-env=FORECREW_WORKER_TOKEN -u "$svc_user" "$@" ;;
	esac
}

create_user() {
	case "$os" in
	Linux)
		if ! getent passwd "$svc_user" >/dev/null 2>&1; then
			say "creating the user $svc_user"
			# bash, not nologin: agents run shell commands as this user. It has no
			# password, so nobody can log in as it.
			useradd --system --home-dir "$state_dir" --create-home --shell /bin/bash "$svc_user"
		fi
		;;
	Darwin)
		if ! dscl . -read "/Users/$svc_user" >/dev/null 2>&1; then
			say "creating the user $svc_user"
			id_num=300
			used=$(dscl . -list /Users UniqueID | awk '{ print $2 }')
			while echo "$used" | grep -qx "$id_num"; do id_num=$((id_num + 1)); done
			dscl . -create "/Groups/$svc_group"
			dscl . -create "/Groups/$svc_group" PrimaryGroupID "$id_num"
			dscl . -create "/Users/$svc_user"
			dscl . -create "/Users/$svc_user" UniqueID "$id_num"
			dscl . -create "/Users/$svc_user" PrimaryGroupID "$id_num"
			dscl . -create "/Users/$svc_user" UserShell /bin/bash
			dscl . -create "/Users/$svc_user" RealName "Forecrew worker"
			dscl . -create "/Users/$svc_user" NFSHomeDirectory "$state_dir"
			dscl . -create "/Users/$svc_user" IsHidden 1
		fi
		;;
	esac
}

create_user
mkdir -p "$conf_dir" "$bin_dir" "$root$state_dir"
chown "$svc_user:$svc_group" "$root$state_dir"
chmod 0755 "$conf_dir"

# The new configuration and token go to temporary files first. They replace
# the installed ones only after "forecrew-worker check" passes.
conf_check="$conf"
if [ "$new_config" -eq 1 ]; then
	{
		echo "server_url: $server"
		[ -z "$name" ] || echo "name: $name"
		if [ -n "$labels" ]; then
			echo "labels: [$(echo "$labels" | sed 's/,/, /g')]"
		fi
		echo "work_dir: $work_dir"
		[ -z "$max_jobs" ] || echo "max_jobs: $max_jobs"
	} >"$conf.tmp"
	chmod 0644 "$conf.tmp"
	conf_check="$conf.tmp"
fi
work_dir=$(awk '$1 == "work_dir:" { print $2 }' "$conf_check")
mkdir -p "$root$work_dir"
chown "$svc_user:$svc_group" "$root$work_dir"

if [ -n "$token" ]; then
	(
		umask 077
		echo "FORECREW_WORKER_TOKEN=$token" >"$token_file.tmp"
	)
	chmod 0600 "$token_file.tmp"
else
	token=$(sed -n 's/^FORECREW_WORKER_TOKEN=//p' "$token_file")
	valid_token "$token" || die "the token in $token_file is not a worker token; pass a new one"
fi
# On Linux systemd reads the file as root, so the service user (and the agents
# that run as it) cannot. launchd runs the service as the service user, which
# has to read it itself: on macOS agents can read the worker token.
case "$os" in
Linux) token_owner=root:root ;;
Darwin) token_owner="$svc_user:$svc_group" ;;
esac
[ ! -e "$token_file.tmp" ] || chown "$token_owner" "$token_file.tmp"

# --- check, then replace the binary -----------------------------------------------

install -m 0755 "$tmp/forecrew-worker" "$bin.new"
say "running forecrew-worker check"
if ! (
	FORECREW_WORKER_TOKEN="$token"
	export FORECREW_WORKER_TOKEN
	run_as_service "$bin.new" check --config "$conf_check"
); then
	die "forecrew-worker check failed; the installed service, configuration and token were not touched. Fix the problem above and run the script again"
fi
if [ "$new_config" -eq 1 ]; then
	say "writing $conf"
	mv "$conf.tmp" "$conf"
fi
if [ -e "$token_file.tmp" ]; then
	say "writing $token_file"
	mv "$token_file.tmp" "$token_file"
fi
# An existing token file may predate the root-only rule.
chown "$token_owner" "$token_file"
chmod 0600 "$token_file"
mv "$bin.new" "$bin"

# --- service -------------------------------------------------------------------

case "$os" in
Linux)
	mkdir -p "$root/etc/systemd/system"
	cat >"$unit" <<UNIT
[Unit]
Description=Forecrew worker
Documentation=https://github.com/forecrew/forecrew
After=network-online.target
Wants=network-online.target

[Service]
User=$svc_user
Group=$svc_group
EnvironmentFile=$token_file
Environment=HOME=$state_dir
Environment=SHELL=/bin/bash
Environment=PATH=/usr/local/bin:/usr/bin:/bin
ExecStart=$bin run --config $conf
Restart=on-failure
RestartSec=5
# SIGTERM goes to the worker alone, so it drains its jobs; whatever is left
# when it exits (or when the timeout ends) is killed.
KillMode=mixed
# The worker lets running jobs finish for 10 minutes on SIGTERM.
TimeoutStopSec=660
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
UNIT
	systemctl daemon-reload
	systemctl enable "$service"
	systemctl restart "$service"
	say "the $service service is running: systemctl status $service"
	;;
Darwin)
	mkdir -p "$root/Library/LaunchDaemons" "$root/var/log"
	touch "$log_file"
	chown "$svc_user:$svc_group" "$log_file"
	cat >"$plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>$launchd_label</string>
	<key>UserName</key>
	<string>$svc_user</string>
	<key>GroupName</key>
	<string>$svc_group</string>
	<key>ProgramArguments</key>
	<array>
		<string>/bin/sh</string>
		<string>-c</string>
		<string>set -a; . $token_file; exec $bin run --config $conf</string>
	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>HOME</key>
		<string>$state_dir</string>
		<key>PATH</key>
		<string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ExitTimeOut</key>
	<integer>660</integer>
	<key>StandardErrorPath</key>
	<string>$log_path</string>
	<key>StandardOutPath</key>
	<string>$log_path</string>
</dict>
</plist>
PLIST
	chmod 0644 "$plist"
	launchctl bootout "system/$launchd_label" 2>/dev/null || true
	launchctl bootstrap system "$plist"
	say "the $launchd_label service is running: launchctl print system/$launchd_label"
	;;
esac
