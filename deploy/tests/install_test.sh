#!/usr/bin/env bash
# Tests deploy/worker/install.sh with mocked systemctl, launchctl, user
# management and download. Everything is written under a temporary root.
# shellcheck disable=SC2016 # the mocks are scripts in single quotes
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
script="$here/../worker/install.sh"
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

# --- mocks -----------------------------------------------------------------

mock() { # mock NAME BODY: a command that logs its call and runs BODY
	printf '#!/bin/sh\necho "%s $*" >>"$MOCK_LOG"\n%s\n' "$1" "$2" >"$tmp/bin/$1"
	chmod +x "$tmp/bin/$1"
}
mock systemctl 'true'
mock launchctl 'true'
mock userdel 'true'
mock chown 'true'
mock useradd 'touch "$MOCK_STATE/user"'
mock getent '[ -e "$MOCK_STATE/user" ]'
mock dscl 'case "$1 $2" in ". -read") [ -e "$MOCK_STATE/user" ] ;; ". -create") touch "$MOCK_STATE/user" ;; esac'
# runuser -u USER -- CMD... and sudo --preserve-env=VAR -u USER CMD...
mock runuser 'shift 3; exec "$@"'
mock sudo 'shift 3; exec "$@"'
cat >"$tmp/bin/curl" <<'MOCK'
#!/bin/sh
# curl [-fsSL] [-K -] [-H header] -o FILE URL: serves FILE's basename from the
# fixtures. It logs the arguments, the Accept header and whether the config
# read from stdin carries an Authorization header (never the header itself).
args="$*"
out="" url="" accept="" auth=no
while [ $# -gt 0 ]; do
	case "$1" in
	-o) out="$2"; shift ;;
	-K) if grep -q '^header = "Authorization: Bearer ghp_secret123"$'; then auth=yes; fi; shift ;;
	-H) case "$2" in Accept:*) accept="${2#Accept: }" ;; esac; shift ;;
	-*) ;;
	*) url="$1" ;;
	esac
	shift
done
echo "curl $url accept=$accept auth=$auth" >>"$MOCK_LOG"
echo "curl-args $args" >>"$MOCK_LOG"
cp "$MOCK_FIXTURES/$(basename "$url")" "$out"
MOCK
chmod +x "$tmp/bin/curl"

# release GOOS ID [check-exit]: publishes a fake release whose binary prints ID
# and exits with check-exit on "check".
release() {
	local goos="$1" id="$2" code="${3:-0}" dir archive
	dir="$(mktemp -d -p "$tmp")"
	archive="forecrew-worker_${goos}_amd64.tar.gz"
	cat >"$dir/forecrew-worker" <<BIN
#!/bin/sh
echo "$id \$* token=\${FORECREW_WORKER_TOKEN:-none}" >>"\$MOCK_LOG"
[ "\$1" != check ] || exit $code
BIN
	chmod +x "$dir/forecrew-worker"
	tar -czf "$MOCK_FIXTURES/$archive" -C "$dir" forecrew-worker
	(cd "$MOCK_FIXTURES" && sha256sum "$archive" >checksums.txt)
	# The same release as the GitHub API shows it: assets are served by ID.
	cp "$MOCK_FIXTURES/$archive" "$MOCK_FIXTURES/1001"
	cp "$MOCK_FIXTURES/checksums.txt" "$MOCK_FIXTURES/1002"
	cat >"$MOCK_FIXTURES/latest" <<JSON
{
  "url": "https://api.github.com/repos/forecrew/forecrew/releases/1",
  "assets": [
    {
      "url": "https://api.github.com/repos/forecrew/forecrew/releases/assets/1002",
      "id": 1002,
      "name": "checksums.txt",
      "browser_download_url": "https://github.com/forecrew/forecrew/releases/download/v/checksums.txt"
    },
    {
      "url": "https://api.github.com/repos/forecrew/forecrew/releases/assets/1001",
      "id": 1001,
      "name": "$archive",
      "browser_download_url": "https://github.com/forecrew/forecrew/releases/download/v/$archive"
    }
  ]
}
JSON
	cp "$MOCK_FIXTURES/latest" "$MOCK_FIXTURES/v2.0.0"
}

conf="$FORECREW_INSTALL_ROOT/etc/forecrew-worker/worker.yaml"
token_file="$FORECREW_INSTALL_ROOT/etc/forecrew-worker/token.env"
bin="$FORECREW_INSTALL_ROOT/usr/local/bin/forecrew-worker"
unit="$FORECREW_INSTALL_ROOT/etc/systemd/system/forecrew-worker.service"
plist="$FORECREW_INSTALL_ROOT/Library/LaunchDaemons/com.forecrew.worker.plist"

reset() { rm -rf "$FORECREW_INSTALL_ROOT" "${MOCK_STATE:?}"/* "$MOCK_LOG"; touch "$MOCK_LOG"; }
logged() { grep -q -- "$1" "$MOCK_LOG" || { cat "$MOCK_LOG" >&2; fail "expected a call matching: $1"; }; }
not_logged() { ! grep -q -- "$1" "$MOCK_LOG" || { cat "$MOCK_LOG" >&2; fail "unexpected call matching: $1"; }; }
mode() { stat -c %a "$1"; }

export FORECREW_INSTALL_OS=Linux
tok=fcw_testtoken

# --- Linux: first install ---------------------------------------------------

reset
release linux v1
"$script" --server https://forecrew.example.com --token $tok --name box --labels go,node,docker --max-jobs 1 >/dev/null

grep -qx 'server_url: https://forecrew.example.com' "$conf" || fail "server_url missing"
grep -qx 'name: box' "$conf" || fail "name missing"
grep -qx 'labels: \[go, node, docker\]' "$conf" || fail "labels missing: $(cat "$conf")"
grep -qx 'work_dir: /var/lib/forecrew-worker/work' "$conf" || fail "work_dir missing"
grep -qx 'max_jobs: 1' "$conf" || fail "max_jobs missing"
! grep -q fcw_ "$conf" || fail "token written to worker.yaml"
[ "$(mode "$token_file")" = 600 ] || fail "token file mode $(mode "$token_file"), want 600"
grep -qx "FORECREW_WORKER_TOKEN=$tok" "$token_file" || fail "token file content"
[ -x "$bin" ] || fail "binary not installed"
[ ! -e "$bin.new" ] || fail "temporary binary left behind"
logged "useradd --system"
logged "chown root:root $token_file.tmp" # agents run as the service user and must not read it
not_logged "chown forecrew-worker:forecrew-worker $token_file"
logged "v1 check --config $conf.tmp token=$tok"
grep -q '^User=forecrew-worker$' "$unit" || fail "unit user"
grep -q "^EnvironmentFile=$token_file\$" "$unit" || fail "unit environment file"
grep -q "^ExecStart=$bin run --config $conf\$" "$unit" || fail "unit ExecStart"
grep -q '^KillMode=mixed$' "$unit" || fail "unit kill mode"
grep -q '^Restart=on-failure$' "$unit" || fail "unit restart policy"
! grep -q fcw_ "$unit" || fail "token in unit"
logged "systemctl enable forecrew-worker"
logged "systemctl restart forecrew-worker"
logged "curl https://github.com/forecrew/forecrew/releases/latest/download/forecrew-worker_linux_amd64.tar.gz"

# --- Linux: rerun upgrades and keeps the config -----------------------------

cp "$conf" "$tmp/conf.before"
cp "$token_file" "$tmp/token.before"
: >"$MOCK_LOG"
release linux v2
"$script" --version v2.0.0 --labels other --server https://elsewhere.example.com >"$tmp/out.txt"
cmp -s "$conf" "$tmp/conf.before" || fail "config changed on upgrade"
cmp -s "$token_file" "$tmp/token.before" || fail "token changed on upgrade"
grep -q 'keeping the existing' "$tmp/out.txt" || fail "no note about the kept config"
logged "v2 check"
"$bin" check >/dev/null
logged "v2 check --config" # the installed binary is the new one
not_logged "useradd"
logged "systemctl restart forecrew-worker"
logged "curl https://github.com/forecrew/forecrew/releases/download/v2.0.0/forecrew-worker_linux_amd64.tar.gz"

# --- Linux: --reconfigure and a new token -----------------------------------

"$script" --reconfigure --server https://elsewhere.example.com --token fcw_other >/dev/null
grep -qx 'server_url: https://elsewhere.example.com' "$conf" || fail "reconfigure ignored"
grep -qx "FORECREW_WORKER_TOKEN=fcw_other" "$token_file" || fail "new token not written"
[ "$(mode "$token_file")" = 600 ] || fail "token file mode after rotation"

# --- Linux: a failing check keeps the installed binary ----------------------

before="$(cat "$bin")"
release linux v3 1
if "$script" >/dev/null 2>"$tmp/err.txt"; then fail "failed check reported success"; fi
grep -q 'check failed' "$tmp/err.txt" || fail "no message for the failed check"
[ "$before" = "$(cat "$bin")" ] || fail "binary replaced after a failed check"
[ ! -e "$bin.new" ] || fail "temporary binary left behind after a failed check"

# --- Linux: a failing check keeps the installed config and token ---------------

cp "$conf" "$tmp/conf.before"
cp "$token_file" "$tmp/token.before"
release linux v4 1
if "$script" --reconfigure --server https://new.example.com --labels rust --token fcw_new >/dev/null 2>"$tmp/err.txt"; then fail "failed check reported success"; fi
cmp -s "$conf" "$tmp/conf.before" || fail "config replaced after a failed check"
cmp -s "$token_file" "$tmp/token.before" || fail "token replaced after a failed check"
[ ! -e "$conf.tmp" ] && [ ! -e "$token_file.tmp" ] || fail "temporary config or token left behind"
logged "v4 check --config $conf.tmp token=fcw_new" # the check saw the candidate config and token

# --- Linux: bad input --------------------------------------------------------

release linux v1
(cd "$MOCK_FIXTURES" && echo "0000  forecrew-worker_linux_amd64.tar.gz" >checksums.txt)
if "$script" >/dev/null 2>"$tmp/err.txt"; then fail "bad checksum accepted"; fi
grep -q 'checksum mismatch' "$tmp/err.txt" || fail "no message for the checksum"
release linux v1

reset
release linux v1
if "$script" --server https://x.example.com >/dev/null 2>"$tmp/err.txt"; then fail "missing token accepted"; fi
grep -q 'token is required' "$tmp/err.txt" || fail "no message for the missing token"
if "$script" --token $tok >/dev/null 2>&1; then fail "missing server accepted"; fi
if "$script" --server https://x.example.com --token nope >/dev/null 2>&1; then fail "bad token accepted"; fi
if "$script" --server 'https://x.example.com;rm' --token $tok >/dev/null 2>&1; then fail "unsafe value accepted"; fi
for bad in 'fcw_x;touch pwned' 'fcw_$(touch pwned)' 'fcw_' 'fcw_a b' "$(printf 'fcw_a\nFOO=bar')"; do
	if "$script" --server https://x.example.com --token "$bad" >/dev/null 2>&1; then fail "malicious token accepted: $bad"; fi
done
[ ! -e pwned ] && [ ! -e "$conf" ] || fail "a malicious token had an effect"
if "$script" --server ftp://x --token $tok >/dev/null 2>&1; then fail "non-http URL accepted"; fi
if "$script" --server https://x.example.com --token $tok --max-jobs 0 >/dev/null 2>&1; then fail "max-jobs 0 accepted"; fi
if "$script" --bogus >/dev/null 2>&1; then fail "unknown option accepted"; fi
if "$script" --purge >/dev/null 2>&1; then fail "--purge alone accepted"; fi
[ ! -e "$conf" ] || fail "config written despite bad input"

# The token may come from the environment.
FORECREW_WORKER_TOKEN=$tok "$script" --server https://x.example.com >/dev/null
grep -qx "FORECREW_WORKER_TOKEN=$tok" "$token_file" || fail "token from the environment"

# --- Linux: uninstall ---------------------------------------------------------

: >"$MOCK_LOG"
"$script" --uninstall >/dev/null
[ ! -e "$unit" ] || fail "unit kept"
[ ! -e "$bin" ] || fail "binary kept"
[ -e "$conf" ] && [ -e "$token_file" ] || fail "config removed without --purge"
logged "systemctl disable --now forecrew-worker"
not_logged userdel
"$script" --uninstall --purge >/dev/null
[ ! -e "$conf" ] && [ ! -e "$token_file" ] || fail "config kept by --purge"
logged "userdel forecrew-worker"

# --- private repository: GITHUB_TOKEN -----------------------------------------

reset
release linux priv
GITHUB_TOKEN=ghp_secret123 "$script" --server https://x.example.com --token $tok >/dev/null
logged "curl https://api.github.com/repos/forecrew/forecrew/releases/latest accept=application/vnd.github+json auth=yes"
logged "curl https://api.github.com/repos/forecrew/forecrew/releases/assets/1001 accept=application/octet-stream auth=yes"
logged "curl https://api.github.com/repos/forecrew/forecrew/releases/assets/1002 accept=application/octet-stream auth=yes"
not_logged "github.com/forecrew/forecrew/releases/latest/download"
not_logged ghp_secret123
! grep -rq ghp_secret123 "$FORECREW_INSTALL_ROOT" || fail "GITHUB_TOKEN written to disk"
logged "priv check"

: >"$MOCK_LOG"
GITHUB_TOKEN=ghp_secret123 "$script" --version v2.0.0 >/dev/null
logged "curl https://api.github.com/repos/forecrew/forecrew/releases/tags/v2.0.0 accept=application/vnd.github+json auth=yes"

# Anonymous downloads send no credentials.
: >"$MOCK_LOG"
"$script" >/dev/null
not_logged "auth=yes"
logged "auth=no"

# The token is not sent to a host other than api.github.com.
: >"$MOCK_LOG"
cp "$MOCK_FIXTURES/1001" "$MOCK_FIXTURES/forecrew-worker_linux_amd64.tar.gz"
cp "$MOCK_FIXTURES/1002" "$MOCK_FIXTURES/checksums.txt"
GITHUB_TOKEN=ghp_secret123 "$script" --base-url https://mirror.example.com/forecrew >"$tmp/out.txt"
not_logged "auth=yes"
logged "curl https://mirror.example.com/forecrew/latest/download/"
grep -q 'ignoring GITHUB_TOKEN' "$tmp/out.txt" || fail "no note about the ignored token"
if GITHUB_TOKEN='bad token;' "$script" >/dev/null 2>&1; then fail "odd GITHUB_TOKEN accepted"; fi

# --- macOS ----------------------------------------------------------------------

export FORECREW_INSTALL_OS=Darwin
reset
release darwin m1
"$script" --server https://forecrew.example.com --token $tok --work-dir /Users/Shared/forecrew-work >/dev/null
grep -qx 'work_dir: /Users/Shared/forecrew-work' "$conf" || fail "macOS work_dir"
[ "$(mode "$token_file")" = 600 ] || fail "macOS token file mode"
logged "dscl . -create /Users/_forecrew-worker UniqueID 300"
logged "m1 check --config $conf.tmp token=$tok"
grep -q '<string>_forecrew-worker</string>' "$plist" || fail "plist user"
grep -q "exec $bin run --config $conf" "$plist" || fail "plist command"
grep -q '<key>KeepAlive</key>' "$plist" || fail "plist keep alive"
! grep -q fcw_ "$plist" || fail "token in plist"
logged "launchctl bootstrap system $plist"
logged "chown _forecrew-worker:_forecrew-worker $token_file.tmp" # launchd runs the service as that user
logged "curl https://github.com/forecrew/forecrew/releases/latest/download/forecrew-worker_darwin_amd64.tar.gz"

: >"$MOCK_LOG"
release darwin m2
"$script" >/dev/null
not_logged "dscl . -create"
logged "launchctl bootout system/com.forecrew.worker"
logged "launchctl bootstrap system $plist"

"$script" --uninstall >/dev/null
[ ! -e "$plist" ] || fail "plist kept"
[ ! -e "$bin" ] || fail "macOS binary kept"
[ -e "$conf" ] || fail "macOS config removed without --purge"

echo "install tests passed"
