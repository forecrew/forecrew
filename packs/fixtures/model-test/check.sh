#!/bin/sh
# Part of the suitability test of a model: records that the agent ran it.
token=$(cat token.txt)
printf '%s\n' "$token" > .check-ran
echo "check: $token"
