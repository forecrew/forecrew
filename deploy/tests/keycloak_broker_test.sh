#!/usr/bin/env bash
# Starts deploy/examples/keycloak-broker and checks that its ID token carries
# what Forecrew reads: sub, a verified email and the groups claim. Needs Docker;
# it pulls Keycloak. COMPOSE_PROJECT_NAME and KEYCLOAK_PORT may be set.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$here/../examples/keycloak-broker"
export COMPOSE_PROJECT_NAME="${COMPOSE_PROJECT_NAME:-keycloak-broker-test}"
export KEYCLOAK_PORT="${KEYCLOAK_PORT:-8180}" KEYCLOAK_ADMIN_PASSWORD="test-$RANDOM"
trap 'docker compose down -v >/dev/null 2>&1 || true' EXIT
fail() { echo "FAIL: $*" >&2; docker compose logs --tail 30 >&2 || true; exit 1; }

docker compose up -d >/dev/null
issuer="http://127.0.0.1:$KEYCLOAK_PORT/realms/forecrew"
for _ in $(seq 1 120); do
	curl -fsS "$issuer/.well-known/openid-configuration" >/dev/null 2>&1 && break
	sleep 2
done
config="$(curl -fsS "$issuer/.well-known/openid-configuration")" || fail "no discovery document"
echo "$config" | grep -q '"S256"' || fail "PKCE S256 is not offered"

token="$(curl -fsS "$issuer/protocol/openid-connect/token" -d grant_type=password -d client_id=forecrew -d client_secret=change-me \
	-d username=ann -d password=change-me -d scope='openid email profile')" || fail "no token"
python3 - "$token" <<'PY' || fail "the ID token lacks a claim"
import base64, json, sys
payload = json.loads(sys.argv[1])["id_token"].split(".")[1]
claims = json.loads(base64.urlsafe_b64decode(payload + "=" * (-len(payload) % 4)))
assert claims["sub"], claims
assert claims["email"] == "ann@acme.example", claims
assert claims["email_verified"] is True, claims
assert claims["groups"] == ["devs"], claims
PY
echo "keycloak broker: ok"
