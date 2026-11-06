#!/bin/sh
# Installs, upgrades or removes forecrew-fleet-agent as a service on Linux
# (systemd) or macOS (launchd). Run it as root, with the code in the
# environment: on a command line it shows up in the process list and in the
# shell history. A join code gives access to nothing until you confirm what the
# agent reports, and no token is shown to anyone:
#
#   read -r FORECREW_JOIN_CODE && export FORECREW_JOIN_CODE
#   sudo --preserve-env=FORECREW_JOIN_CODE,GITHUB_TOKEN sh install.sh \
#     --server https://forecrew.example.com
#
# It works out the kind of location itself and asks here for what that kind
# needs locally; nothing of that leaves this machine. See
# docs/runbooks/add-provider.md. Run it again to upgrade the binary: the
# existing configuration is kept.
set -eu

usage() {
	cat <<'USAGE'
usage: install.sh [options]
       install.sh --uninstall [--purge]

  --server URL      forecrewd's public URL (required on the first run)
  --kind KIND       microvm, proxmox, hyperv, tart or aws (default: detected)
  --version TAG     release to install, for example v0.2.0 (default: latest)
  --base-url URL    release location (default https://github.com/forecrew/forecrew/releases)
  --reconfigure     rewrite agent.yaml from these options; otherwise an
                    existing agent.yaml is kept
  --uninstall       stop and remove the service and the binary; keeps the
                    configuration and the token, and leaves machines running
  --purge           with --uninstall: also delete the configuration, the token,
                    /var/lib/forecrew-fleet-agent and the service user

Credentials come from the environment, never from a flag:
  FORECREW_JOIN_CODE       from "forecrew provider join"
  FORECREW_PROVIDER_TOKEN  from "forecrew provider add", for scripts and
                           air-gapped sites
  FORECREW_PROXMOX_TOKEN   the Proxmox API token, where nobody can be asked

Values may contain letters, digits and . _ / : @ + - only.
USAGE
}

die() {
	echo "install.sh: $*" >&2
	exit 1
}

say() { echo "install.sh: $*"; }

# The secrets file is read by sh on macOS, so nothing but these characters may
# reach it.
valid_token() {
	case "$1" in
	fcp_) return 1 ;;
	fcp_*[!A-Za-z0-9_-]*) return 1 ;;
	fcp_*) return 0 ;;
	*) return 1 ;;
	esac
}

valid_code() {
	case "$1" in
	"" | *[!A-Za-z0-9-]*) return 1 ;;
	*) return 0 ;;
	esac
}

server="" kind="" version="latest" base_url="https://github.com/forecrew/forecrew/releases"
reconfigure=0 uninstall=0 purge=0
token="${FORECREW_PROVIDER_TOKEN:-}" code="${FORECREW_JOIN_CODE:-}"

while [ $# -gt 0 ]; do
	case "$1" in
	-h | --help) usage; exit 0 ;;
	--uninstall) uninstall=1; shift; continue ;;
	--purge) purge=1; shift; continue ;;
	--reconfigure) reconfigure=1; shift; continue ;;
	--server | --kind | --version | --base-url)
		[ $# -ge 2 ] || die "$1 needs a value"
		case "$1" in
		--server) server="$2" ;;
		--kind) kind="$2" ;;
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
bin="$bin_dir/forecrew-fleet-agent"
conf_dir="$root/etc/forecrew-fleet-agent"
conf="$conf_dir/agent.yaml"
secrets="$conf_dir/secrets.env"
state_dir="/var/lib/forecrew-fleet-agent"
token_path="$state_dir/token"
service="forecrew-fleet-agent"
launchd_label="com.forecrew.fleet-agent"
case "$os" in
Linux)
	svc_user="forecrew-fleet-agent"
	svc_group="forecrew-fleet-agent"
	unit="$root/etc/systemd/system/$service.service"
	;;
Darwin)
	svc_user="_forecrew-fleet-agent"
	svc_group="_forecrew-fleet-agent"
	plist="$root/Library/LaunchDaemons/$launchd_label.plist"
	log_path=/var/log/forecrew-fleet-agent.log
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
		say "deleted the configuration, the token, $state_dir and the user $svc_user"
	else
		say "kept $conf_dir; add --purge to delete it"
	fi
	exit 0
fi

# --- validate the arguments ------------------------------------------------

[ "$purge" -eq 0 ] || die "--purge only goes with --uninstall"
check_value --server "$server"
check_value --kind "$kind"
check_value --version "$version"
check_value --base-url "$base_url"

new_config=1
if [ -f "$conf" ] && [ "$reconfigure" -eq 0 ]; then
	new_config=0
	if [ -n "$server$kind" ]; then
		say "keeping the existing $conf; add --reconfigure to rewrite it"
	fi
fi
if [ "$new_config" -eq 1 ]; then
	[ -n "$server" ] || die "--server is required"
	case "$server" in
	http://* | https://*) ;;
	*) die "--server must be an http:// or https:// URL" ;;
	esac
fi
[ -z "$token" ] || [ -z "$code" ] || die "set FORECREW_JOIN_CODE or FORECREW_PROVIDER_TOKEN, not both"
if [ -n "$token" ]; then
	valid_token "$token" || die "the provider token must be fcp_ followed by letters, digits, - and _ (forecrew provider add prints it)"
elif [ -n "$code" ]; then
	valid_code "$code" || die "a join code has letters, digits and - only (forecrew provider join prints it)"
elif [ ! -f "$secrets" ] && [ ! -f "$root$token_path" ]; then
	die "set FORECREW_JOIN_CODE (forecrew provider join) or FORECREW_PROVIDER_TOKEN (forecrew provider add)"
fi

# --- download ----------------------------------------------------------------

case "${FORECREW_INSTALL_ARCH:-$(uname -m)}" in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) die "unsupported architecture $(uname -m)" ;;
esac
goos=$(echo "$os" | tr '[:upper:]' '[:lower:]')
archive="forecrew-fleet-agent_${goos}_${arch}.tar.gz"
if [ "$version" = latest ]; then
	url_dir="$base_url/latest/download"
else
	url_dir="$base_url/download/$version"
fi

tmp=$(mktemp -d)
cleanup() {
	rm -rf "$tmp"
	rm -f "$bin.new" "$conf.tmp" "$secrets.tmp"
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
tar -xzf "$tmp/$archive" -C "$tmp" forecrew-fleet-agent || die "cannot unpack $archive"

# --- the kind of location -------------------------------------------------------

if [ "$new_config" -eq 1 ]; then
	if [ -z "$kind" ]; then
		kind=$("$tmp/forecrew-fleet-agent" detect) || die "this machine fits no kind of location by itself; pass --kind"
		say "this machine looks like kind $kind; pass --kind to choose another"
	fi
else
	kind=$(awk '$1 == "kind:" { print $2 }' "$conf")
fi

# --- user, files ---------------------------------------------------------------

# run_as_service COMMAND...: runs a command as the service user.
run_as_service() {
	case "$os" in
	Linux) runuser -u "$svc_user" -- "$@" ;;
	Darwin) sudo -u "$svc_user" "$@" ;;
	esac
}

create_user() {
	case "$os" in
	Linux)
		if ! getent passwd "$svc_user" >/dev/null 2>&1; then
			say "creating the user $svc_user"
			useradd --system --home-dir "$state_dir" --create-home --shell /usr/sbin/nologin "$svc_user"
		fi
		# microVMs need /dev/kvm, which its group opens.
		if getent group kvm >/dev/null 2>&1; then
			usermod -aG kvm "$svc_user"
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
			dscl . -create "/Users/$svc_user" UserShell /usr/bin/false
			dscl . -create "/Users/$svc_user" RealName "Forecrew fleet agent"
			dscl . -create "/Users/$svc_user" NFSHomeDirectory "$state_dir"
			dscl . -create "/Users/$svc_user" IsHidden 1
		fi
		;;
	esac
}

create_user
mkdir -p "$conf_dir" "$bin_dir" "$root$state_dir"
chown "$svc_user:$svc_group" "$root$state_dir"
chmod 0700 "$root$state_dir"
chmod 0755 "$conf_dir"

# ask_local: what this kind needs here. It goes to a file only the agent's user
# reads; forecrewd never sees it. A kind not named needs nothing, or finds it by
# itself, as the instance role of aws.
ask_local() {
	case "$kind" in
	proxmox)
		file="$conf_dir/proxmox-token"
		value="${FORECREW_PROXMOX_TOKEN:-}"
		if [ -z "$value" ] && [ ! -f "$file" ] && [ -t 0 ]; then
			printf 'Proxmox API token (user@realm!name=secret), not shown: '
			stty -echo
			read -r value || true
			stty echo
			echo
		fi
		if [ -n "$value" ]; then
			(
				umask 077
				printf '%s\n' "$value" >"$file"
			)
			chown "$svc_user:$svc_group" "$file"
			chmod 0600 "$file"
			say "wrote $file, readable by $svc_user alone"
		elif [ ! -f "$file" ]; then
			say "warning: no Proxmox API token: put one in $file, readable by $svc_user alone"
		fi
		;;
	esac
}
ask_local

# The new configuration and secrets go to temporary files first. They replace
# the installed ones only after "forecrew-fleet-agent doctor" passes.
if [ "$new_config" -eq 1 ]; then
	{
		echo "server_url: $server"
		echo "kind: $kind"
		echo "token_file: $token_path"
	} >"$conf.tmp"
	chmod 0644 "$conf.tmp"
fi
if [ -n "$token$code" ]; then
	(
		umask 077
		if [ -n "$token" ]; then
			echo "FORECREW_PROVIDER_TOKEN=$token" >"$secrets.tmp"
		else
			echo "FORECREW_JOIN_CODE=$code" >"$secrets.tmp"
		fi
	)
	chmod 0600 "$secrets.tmp"
fi
# On Linux systemd reads the file as root. launchd runs the service as the
# service user, which has to read it itself.
case "$os" in
Linux) secrets_owner=root:root ;;
Darwin) secrets_owner="$svc_user:$svc_group" ;;
esac
[ ! -e "$secrets.tmp" ] || chown "$secrets_owner" "$secrets.tmp"

# --- check, then replace the binary -----------------------------------------------

install -m 0755 "$tmp/forecrew-fleet-agent" "$bin.new"
say "running forecrew-fleet-agent doctor"
if ! run_as_service "$bin.new" doctor --kind "$kind"; then
	die "forecrew-fleet-agent doctor found something missing; the installed service, configuration and token were not touched. Fix the problem above and run the script again"
fi
if [ "$new_config" -eq 1 ]; then
	say "writing $conf"
	mv "$conf.tmp" "$conf"
fi
if [ -e "$secrets.tmp" ]; then
	say "writing $secrets"
	mv "$secrets.tmp" "$secrets"
	# The credentials given now are the ones to use.
	rm -f "$root$token_path"
fi
if [ ! -f "$secrets" ]; then
	(
		umask 077
		: >"$secrets"
	)
fi
chown "$secrets_owner" "$secrets"
chmod 0600 "$secrets"
mv "$bin.new" "$bin"

# --- service -------------------------------------------------------------------

case "$os" in
Linux)
	mkdir -p "$root/etc/systemd/system"
	cat >"$unit" <<UNIT
[Unit]
Description=Forecrew fleet agent
Documentation=https://github.com/forecrew/forecrew
After=network-online.target
Wants=network-online.target

[Service]
User=$svc_user
Group=$svc_group
EnvironmentFile=$secrets
Environment=HOME=$state_dir
Environment=PATH=/usr/local/bin:/usr/bin:/bin
ExecStart=$bin run --config $conf
# A revoked token ends it with status 0, and no restart would help then.
Restart=on-failure
RestartSec=5
# Machines outlive the agent: only the agent itself is stopped.
KillMode=process
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
		<string>set -a; . $secrets; exec $bin run --config $conf</string>
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
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
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
if [ -n "$code" ]; then
	say "now confirm it: forecrew provider joins, then forecrew provider confirm <id> --name <name>"
fi
