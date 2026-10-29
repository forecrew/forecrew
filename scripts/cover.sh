#!/usr/bin/env bash
# Fails if any package under internal/ has less than MIN_COVERAGE percent
# statement coverage. Packages without statements (interfaces only) are skipped.
set -euo pipefail

min="${MIN_COVERAGE:-80}"
if [ -z "$(go list ./internal/... 2>/dev/null)" ]; then
  echo "no packages under internal/"
  exit 0
fi
out="$(go test -cover ./internal/... 2>&1)" || { echo "$out"; exit 1; }
echo "$out"

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
