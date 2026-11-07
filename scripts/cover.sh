#!/usr/bin/env bash
# Fails if any package under internal/ or ee/ has less than MIN_COVERAGE percent
# statement coverage, in the open-core build and in the enterprise build.
# Packages without statements (interfaces only) are skipped.
# Set FORECREW_TEST_DATABASE_URL to include the Postgres integration tests.
set -euo pipefail

min="${MIN_COVERAGE:-80}"
if [ -z "$(go list ./internal/... 2>/dev/null)" ]; then
  echo "no packages under internal/"
  exit 0
fi
# The Postgres store is only covered by its integration tests, so they run
# when FORECREW_TEST_DATABASE_URL is set (CI's integration job sets it).
tags=""
if [ -n "${FORECREW_TEST_DATABASE_URL:-}" ]; then tags="-tags integration"; fi
# shellcheck disable=SC2086
out="$(go test $tags -cover ./internal/... 2>&1)" || { echo "$out"; exit 1; }
echo "$out"
enterprise="enterprise"
if [ -n "$tags" ]; then enterprise="integration,enterprise"; fi
ee="$(go test -tags "$enterprise" -cover ./internal/... ./ee/... 2>&1)" || { echo "$ee"; exit 1; }
echo "$ee"
out="$out
$ee"

echo "$out" | awk -v min="$min" '
  /coverage: \[no statements\]/ { next }
  /coverage: [0-9.]+% of statements/ {
    for (i = 1; i <= NF; i++) if ($i == "coverage:") pct = $(i + 1)
    sub("%", "", pct)
    pkg = ($1 == "ok") ? $2 : $1
    if (pct + 0 < min) { printf "FAIL coverage %s%% < %s%%: %s\n", pct, min, pkg; bad = 1 }
  }
  END { exit bad }
'
