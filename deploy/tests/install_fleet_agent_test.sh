#!/usr/bin/env bash
# Tests deploy/fleet-agent/install.sh with mocked systemctl, launchctl, user
# management and download. Everything is written under a temporary root.
# shellcheck disable=SC2016 # the mocks are scripts in single quotes
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
script="$here/../fleet-agent/install.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

export FORECREW_INSTALL_ROOT="$tmp/root"
export FORECREW_INSTALL_ARCH=x86_64
export MOCK_LOG="$tmp/calls.log"
export MOCK_STATE="$tmp/state"
export MOCK_FIXTURES="$tmp/fixtures"
mkdir -p "$tmp/bin" "$MOCK_STATE" "$MOCK_FIXTURES"
export PATH="$tmp/bin:$PATH"

mock() { # mock NAME BODY: a command that logs its call and runs BODY
	printf '#!/bin/sh\necho "%s $*" >>"$MOCK_LOG"\n%s\n' "$1" "$2" >"$tmp/bin/$1"
	chmod +x "$tmp/bin/$1"
}
mock systemctl 'true'
mock launchctl 'true'
mock userdel 'true'
mock usermod 'true'
mock chown 'true'
mock useradd 'touch "$MOCK_STATE/user"'
mock getent '[ -e "$MOCK_STATE/user" ]'
mock dscl 'case "$1 $2" in ". -read") [ -e "$MOCK_STATE/user" ] ;; ". -create") touch "$MOCK_STATE/user" ;; esac'
mock runuser 'shift 3; exec "$@"' # runuser -u USER -- CMD...
mock sudo 'shift 2; exec "$@"'    # sudo -u USER CMD...
cat >"$tmp/bin/curl" <<'MOCK'
#!/bin/sh
out="" url=""
while [ $# -gt 0 ]; do
	case "$1" in
	-o) out="$2"; shift ;;
	-*) ;;
	*) url="$1" ;;
	esac
	shift
done
echo "curl $url" >>"$MOCK_LOG"
cp "$MOCK_FIXTURES/$(basename "$url")" "$out"
MOCK
chmod +x "$tmp/bin/curl"

# release GOOS ID [DETECTED] [DOCTOR-EXIT]: publishes a fake release.
release() {
	local goos="$1" id="$2" detected="${3-microvm}" code="${4:-0}" dir archive
	dir="$(mktemp -d -p "$tmp")"
	archive="forecrew-fleet-agent_${goos}_amd64.tar.gz"
	cat >"$dir/forecrew-fleet-agent" <<BIN
#!/bin/sh
echo "$id \$*" >>"\$MOCK_LOG"
case "\$1" in
detect) [ -n "$detected" ] || exit 1; echo "$detected" ;;
doctor) exit $code ;;
esac
BIN
	chmod +x "$dir/forecrew-fleet-agent"
	tar -czf "$MOCK_FIXTURES/$archive" -C "$dir" forecrew-fleet-agent
	(cd "$MOCK_FIXTURES" && sha256sum "$archive" >checksums.txt)
}

conf_dir="$FORECREW_INSTALL_ROOT/etc/forecrew-fleet-agent"
conf="$conf_dir/agent.yaml"
secrets="$conf_dir/secrets.env"
bin="$FORECREW_INSTALL_ROOT/usr/local/bin/forecrew-fleet-agent"
unit="$FORECREW_INSTALL_ROOT/etc/systemd/system/forecrew-fleet-agent.service"
plist="$FORECREW_INSTALL_ROOT/Library/LaunchDaemons/com.forecrew.fleet-agent.plist"
token_file="$FORECREW_INSTALL_ROOT/var/lib/forecrew-fleet-agent/token"

reset() { rm -rf "$FORECREW_INSTALL_ROOT" "${MOCK_STATE:?}"/* "$MOCK_LOG"; touch "$MOCK_LOG"; }
logged() { grep -q -- "$1" "$MOCK_LOG" || { cat "$MOCK_LOG" >&2; fail "expected a call matching: $1"; }; }
not_logged() { ! grep -q -- "$1" "$MOCK_LOG" || { cat "$MOCK_LOG" >&2; fail "unexpected call matching: $1"; }; }
mode() { stat -c %a "$1"; }
refused() { # refused WANTED-TEXT COMMAND...: the command fails, saying so
	local want="$1" out
	shift
	if out="$("$@" 2>&1)"; then fail "passed, wanted: $want"; fi
	grep -q -- "$want" <<<"$out" || fail "said: $out; wanted: $want"
}

export FORECREW_INSTALL_OS=Linux
code=K7PQ-2M4X
tok=fcp_testtoken

# --- Linux: first install with a join code; the kind is detected ---------------

reset
release linux v1
FORECREW_JOIN_CODE=$code "$script" --server https://forecrew.example.com </dev/null >"$tmp/out"

grep -q 'looks like kind microvm' "$tmp/out" || fail "the detected kind is not said"
grep -q 'forecrew provider confirm' "$tmp/out" || fail "the next step is not said"
grep -qx 'server_url: https://forecrew.example.com' "$conf" || fail "server_url missing"
grep -qx 'kind: microvm' "$conf" || fail "kind missing"
grep -qx 'token_file: /var/lib/forecrew-fleet-agent/token' "$conf" || fail "token_file missing"
! grep -q "$code" "$conf" || fail "the code is in agent.yaml"
[ "$(mode "$secrets")" = 600 ] || fail "secrets mode $(mode "$secrets"), want 600"
grep -qx "FORECREW_JOIN_CODE=$code" "$secrets" || fail "secrets content"
[ -x "$bin" ] || fail "binary not installed"
[ ! -e "$bin.new" ] || fail "temporary binary left behind"
[ "$(mode "$FORECREW_INSTALL_ROOT/var/lib/forecrew-fleet-agent")" = 700 ] || fail "state directory mode"
logged "useradd --system"
logged "usermod -aG kvm"
logged "chown root:root $secrets.tmp"
logged "v1 doctor --kind microvm"
grep -q '^User=forecrew-fleet-agent$' "$unit" || fail "unit user"
grep -q "^EnvironmentFile=$secrets\$" "$unit" || fail "unit environment file"
grep -q "^ExecStart=$bin run --config $conf\$" "$unit" || fail "unit ExecStart"
grep -q '^Restart=on-failure$' "$unit" || fail "unit restart policy"
grep -q '^KillMode=process$' "$unit" || fail "machines must outlive the agent"
! grep -q "$code" "$unit" || fail "the code is in the unit"
logged "systemctl restart forecrew-fleet-agent"

# --- upgrade: configuration, secrets and a token from the join all stay ------------

mkdir -p "$(dirname "$token_file")"
echo fcp_joined >"$token_file"
release linux v2
: >"$MOCK_LOG"
"$script" </dev/null >/dev/null
grep -qx 'kind: microvm' "$conf" || fail "kind lost in an upgrade"
grep -qx "FORECREW_JOIN_CODE=$code" "$secrets" || fail "secrets lost in an upgrade"
[ -e "$token_file" ] || fail "an upgrade dropped the token"
logged "v2 doctor --kind microvm"
not_logged "v2 detect"

# --- new credentials replace the token kept -------------------------------------

FORECREW_PROVIDER_TOKEN=$tok "$script" </dev/null >/dev/null
grep -qx "FORECREW_PROVIDER_TOKEN=$tok" "$secrets" || fail "new token not written"
[ ! -e "$token_file" ] || fail "the old token stayed"

# --- doctor fails: nothing is touched ---------------------------------------------

reset
release linux v1 microvm 1
refused "found something missing" env FORECREW_JOIN_CODE=$code "$script" --server https://forecrew.example.com
[ ! -e "$conf" ] || fail "configuration written after a failed doctor"
[ ! -e "$secrets" ] || fail "secrets written after a failed doctor"
[ ! -e "$bin" ] || fail "binary installed after a failed doctor"
not_logged "systemctl"
[ ! -e "$conf.tmp" ] || fail "temporary configuration left behind"
[ ! -e "$secrets.tmp" ] || fail "temporary secrets left behind"

# --- a machine of no kind ------------------------------------------------------------

reset
release linux v1 ""
refused "pass --kind" env FORECREW_JOIN_CODE=$code "$script" --server https://forecrew.example.com
FORECREW_JOIN_CODE=$code "$script" --server https://forecrew.example.com --kind aws </dev/null >/dev/null
grep -qx 'kind: aws' "$conf" || fail "--kind ignored"

# --- local credentials stay local ---------------------------------------------------

reset
release linux v1 proxmox
FORECREW_JOIN_CODE=$code FORECREW_PROXMOX_TOKEN='root@pam!agent=abc-123' "$script" --server https://forecrew.example.com </dev/null >/dev/null
[ "$(cat "$conf_dir/proxmox-token")" = 'root@pam!agent=abc-123' ] || fail "proxmox token not written"
[ "$(mode "$conf_dir/proxmox-token")" = 600 ] || fail "proxmox token mode"
logged "chown forecrew-fleet-agent:forecrew-fleet-agent $conf_dir/proxmox-token"
! grep -q 'abc-123' "$conf" "$secrets" "$unit" "$MOCK_LOG" || fail "the proxmox token leaked"

reset
out="$(FORECREW_JOIN_CODE=$code "$script" --server https://forecrew.example.com --kind proxmox </dev/null)"
grep -q 'no Proxmox API token' <<<"$out" || fail "a missing proxmox token is not said"

# --- refusals ---------------------------------------------------------------------

reset
release linux v1
refused "set FORECREW_JOIN_CODE" "$script" --server https://forecrew.example.com
refused "not both" env FORECREW_JOIN_CODE=$code FORECREW_PROVIDER_TOKEN=$tok "$script" --server https://x.example.com
refused "must be fcp_" env FORECREW_PROVIDER_TOKEN=fcw_wrong "$script" --server https://x.example.com
refused "must be fcp_" env 'FORECREW_PROVIDER_TOKEN=fcp_a;rm' "$script" --server https://x.example.com
refused "a join code" env 'FORECREW_JOIN_CODE=a b' "$script" --server https://x.example.com
refused "--server is required" env FORECREW_JOIN_CODE=$code "$script"
refused "must be an http" env FORECREW_JOIN_CODE=$code "$script" --server ftp://x
refused "may contain" env FORECREW_JOIN_CODE=$code "$script" --server 'https://x;y'
refused "--purge only" env FORECREW_JOIN_CODE=$code "$script" --purge
refused "unknown option" "$script" --nonsense
FORECREW_INSTALL_OS=Plan9 refused "unsupported operating system" "$script"
rm "$MOCK_FIXTURES/checksums.txt"; touch "$MOCK_FIXTURES/checksums.txt"
refused "no entry" env FORECREW_JOIN_CODE=$code "$script" --server https://x.example.com
release linux v1
sed -i 's/^./0/' "$MOCK_FIXTURES/checksums.txt"
refused "checksum mismatch" env FORECREW_JOIN_CODE=$code "$script" --server https://x.example.com

# --- uninstall ------------------------------------------------------------------------

reset
release linux v1
FORECREW_JOIN_CODE=$code "$script" --server https://forecrew.example.com </dev/null >/dev/null
"$script" --uninstall >/dev/null
[ ! -e "$bin" ] || fail "binary left"
[ ! -e "$unit" ] || fail "unit left"
[ -e "$conf" ] || fail "uninstall deleted the configuration"
logged "systemctl disable --now forecrew-fleet-agent"
"$script" --uninstall --purge >/dev/null
[ ! -e "$conf_dir" ] || fail "purge left the configuration"
logged "userdel forecrew-fleet-agent"

# --- macOS ----------------------------------------------------------------------------

export FORECREW_INSTALL_OS=Darwin
reset
release darwin v1 tart
FORECREW_JOIN_CODE=$code "$script" --server https://forecrew.example.com </dev/null >/dev/null
grep -qx 'kind: tart' "$conf" || fail "kind missing on macOS"
logged "chown _forecrew-fleet-agent:_forecrew-fleet-agent $secrets.tmp"
grep -q 'com.forecrew.fleet-agent' "$plist" || fail "plist label"
grep -q '<key>SuccessfulExit</key>' "$plist" || fail "a revoked agent would be restarted for ever"
! grep -q "$code" "$plist" || fail "the code is in the plist"
logged "launchctl bootstrap system $plist"
"$script" --uninstall --purge >/dev/null
[ ! -e "$plist" ] || fail "plist left"
logged 'dscl . -delete /Users/_forecrew-fleet-agent'

echo "ok: fleet agent install script"
