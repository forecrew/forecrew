#!/bin/sh
# Part of the suitability test of a model. Its line comes from token.txt
# and a per-run value only the run's environment holds, so writing it shows
# the agent used a shell in this run.
set -e
token=$(cat token.txt)
if command -v sha256sum >/dev/null 2>&1; then
	sum=$(printf '%s%s' "$token" "$FORECREW_CHECK_SALT" | sha256sum)
else
	sum=$(printf '%s%s' "$token" "$FORECREW_CHECK_SALT" | shasum -a 256)
fi
sum=$(printf '%s' "$sum" | cut -c1-16)
printf '%s\n' "$sum" > .check-ran
echo "check: $sum"
