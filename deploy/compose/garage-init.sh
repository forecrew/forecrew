#!/bin/sh
# Prepares the Garage of the Compose stack: a one-node layout, the evidence
# bucket and the access key forecrewd uses. Run by the garage-init service on
# every start; each step is skipped when it is already done.
#
# Environment: GARAGE_ADMIN_TOKEN, FORECREW_EVIDENCE_ACCESS_KEY_ID,
# FORECREW_EVIDENCE_SECRET_ACCESS_KEY; optional GARAGE_ADMIN_URL
# (default http://garage:3903) and EVIDENCE_BUCKET (default forecrew-evidence).
# Uses the Garage admin API v2 and never prints the secrets.
set -eu

api="${GARAGE_ADMIN_URL:-http://garage:3903}/v2"
bucket="${EVIDENCE_BUCKET:-forecrew-evidence}"
body="$(mktemp)"
auth="$(mktemp)"
trap 'rm -f "$body" "$auth"' EXIT
# The token goes to curl in a file, not on its command line, where other
# processes could read it.
printf 'Authorization: Bearer %s\n' "$GARAGE_ADMIN_TOKEN" >"$auth"

# call METHOD PATH [JSON]: the answer is in $body, its HTTP status in $code
# (000 when the server cannot be reached).
call() {
	code="$(curl -sS -o "$body" -w '%{http_code}' -X "$1" -H "@$auth" \
		${3:+-H 'Content-Type: application/json' -d "$3"} "$api/$2" 2>/dev/null)" || code=000
}

# field NAME: the first value of a JSON string or number field in $body.
field() {
	sed -n "s/.*\"$1\": *\"\{0,1\}\([0-9A-Za-z_-]*\)\"\{0,1\},\{0,1\}\$/\1/p" "$body" | head -n 1
}

# fail WHAT: stops with the HTTP status and the answer's message.
fail() {
	echo "garage-init: $1 failed (HTTP $code): $(sed -n 's/.*"message": *"\(.*\)".*/\1/p' "$body" | head -n 1)" >&2
	exit 1
}

for _ in $(seq 60); do
	call GET GetClusterStatus
	[ "$code" = 200 ] && break
	sleep 1
done
[ "$code" = 200 ] || fail "waiting for Garage"

if [ "$(field layoutVersion)" = 0 ]; then
	node="$(field id)"
	call POST UpdateClusterLayout "{\"roles\":[{\"id\":\"$node\",\"zone\":\"dc1\",\"capacity\":1073741824,\"tags\":[]}]}"
	[ "$code" = 200 ] || fail "assigning the layout"
	call POST ApplyClusterLayout '{"version":1}'
	[ "$code" = 200 ] || fail "applying the layout"
	echo "garage-init: layout applied"
fi

call GET "GetBucketInfo?globalAlias=$bucket"
if [ "$code" = 404 ]; then
	call POST CreateBucket "{\"globalAlias\":\"$bucket\"}"
	echo "garage-init: bucket $bucket created"
fi
{ [ "$code" = 200 ] || [ "$code" = 409 ]; } || fail "creating the bucket"
call GET "GetBucketInfo?globalAlias=$bucket"
[ "$code" = 200 ] || fail "looking up the bucket"
bucket_id="$(field id)"

# A key that exists already answers 409 (it was imported on an earlier start).
call POST ImportKey "{\"name\":\"forecrew\",\"accessKeyId\":\"$FORECREW_EVIDENCE_ACCESS_KEY_ID\",\"secretAccessKey\":\"$FORECREW_EVIDENCE_SECRET_ACCESS_KEY\"}"
{ [ "$code" = 200 ] || [ "$code" = 409 ]; } || fail "importing the access key"

call POST AllowBucketKey "{\"bucketId\":\"$bucket_id\",\"accessKeyId\":\"$FORECREW_EVIDENCE_ACCESS_KEY_ID\",\"permissions\":{\"read\":true,\"write\":true,\"owner\":false}}"
[ "$code" = 200 ] || fail "allowing the key on the bucket"
echo "garage-init: bucket $bucket ready"
