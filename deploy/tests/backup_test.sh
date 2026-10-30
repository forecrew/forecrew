#!/usr/bin/env bash
# Tests deploy/backup/backup.sh with a fake pg_dump.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
script="$here/../backup/backup.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

mkdir "$tmp/bin" "$tmp/out"
cat >"$tmp/bin/pg_dump" <<'MOCK'
#!/bin/sh
for arg in "$@"; do
	case "$arg" in --file=*) out="${arg#--file=}" ;; esac
done
[ -z "${FAKE_PG_DUMP_FAIL:-}" ] || { echo "pg_dump: boom" >&2; : >"$out"; exit 1; }
echo "dump of $*" >"$out"
MOCK
chmod +x "$tmp/bin/pg_dump"
export PATH="$tmp/bin:$PATH" BACKUP_DIR="$tmp/out" BACKUP_RETAIN_DAYS=3

fail() { echo "FAIL: $*" >&2; exit 1; }

# An old dump is removed, a recent one stays, and a new one is written.
touch -d '5 days ago' "$tmp/out/forecrew-old.dump"
touch -d '1 day ago' "$tmp/out/forecrew-recent.dump"
touch "$tmp/out/unrelated.txt"
"$script" >/dev/null
[ ! -e "$tmp/out/forecrew-old.dump" ] || fail "old dump kept"
[ -e "$tmp/out/forecrew-recent.dump" ] || fail "recent dump removed"
[ -e "$tmp/out/unrelated.txt" ] || fail "unrelated file removed"
new="$(find "$tmp/out" -name 'forecrew-2*.dump')"
[ -n "$new" ] || fail "no new dump"
[ "$(stat -c %a "$new")" = 600 ] || fail "dump mode is $(stat -c %a "$new"), want 600"

# The password reaches pg_dump through the environment, never its arguments,
# and a connection string with a password is not used.
cat >"$tmp/bin/pg_dump" <<'MOCK'
#!/bin/sh
{ echo "args: $*"; echo "password: ${PGPASSWORD:-}"; } >"$(dirname "$0")/../pg_dump.seen"
for arg in "$@"; do
	case "$arg" in --file=*) : >"${arg#--file=}" ;; esac
done
MOCK
PGPASSWORD=s3cret FORECREW_DATABASE_URL=postgres://u:urlpass@h/db "$script" >/dev/null
grep -q 'password: s3cret' "$tmp/pg_dump.seen" || fail "PGPASSWORD not passed on"
! grep -q 'args:.*\(s3cret\|urlpass\|postgres://\)' "$tmp/pg_dump.seen" || fail "password or URL in the pg_dump arguments"

cat >"$tmp/bin/pg_dump" <<'MOCK'
#!/bin/sh
for arg in "$@"; do
	case "$arg" in --file=*) out="${arg#--file=}" ;; esac
done
[ -z "${FAKE_PG_DUMP_FAIL:-}" ] || { echo "pg_dump: boom" >&2; : >"$out"; exit 1; }
echo "dump of $*" >"$out"
MOCK

# A failed dump leaves nothing behind and fails the run.
before="$(find "$tmp/out" -type f | sort)"
if FAKE_PG_DUMP_FAIL=1 "$script" 2>/dev/null; then fail "failed dump reported success"; fi
[ "$before" = "$(find "$tmp/out" -type f | sort)" ] || fail "failed dump left files"

# A bad retention is refused.
if BACKUP_RETAIN_DAYS=abc "$script" 2>/dev/null; then fail "bad retention accepted"; fi

echo "backup tests passed"
