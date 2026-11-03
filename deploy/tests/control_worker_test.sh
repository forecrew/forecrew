#!/usr/bin/env bash
# Starts the Compose stack from this checkout under its own project name and
# checks the control worker: it registers in pool control, comes online with
# its runtime labels, cannot reach Postgres, and its token is hidden from the
# agents' user. Needs Docker with the Compose plugin and openssl.
# It writes .env, garage.env and forecrew.yaml into deploy/compose and removes
# them again.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
dir="$(cd "$here/../compose" && pwd)"
export COMPOSE_PROJECT_NAME="${COMPOSE_PROJECT_NAME:-forecrew-control-test}"
export COMPOSE_FILE=docker-compose.yml:docker-compose.build.yml
export FORECREW_HTTP_PORT="${FORECREW_HTTP_PORT:-18080}"
export FORECREW_HTTPS_PORT="${FORECREW_HTTPS_PORT:-18443}"

fail() { echo "FAIL: $*" >&2; exit 1; }

cd "$dir"
[ ! -e .env ] || fail "$dir/.env exists; move it away first"
[ ! -e garage.env ] || fail "$dir/garage.env exists; move it away first"
[ ! -e forecrew.yaml ] || fail "$dir/forecrew.yaml exists; move it away first"

cleanup() {
	docker compose down -v --remove-orphans >/dev/null 2>&1 || true
	rm -f "$dir/.env" "$dir/garage.env" "$dir/forecrew.yaml"
}
trap cleanup EXIT

# Test values only: the stack runs on this host and sends nothing outside it.
key="$(openssl genrsa -traditional 2048 2>/dev/null | awk 'NF{printf "%s\\n", $0}')"
{
	echo "POSTGRES_PASSWORD=control-test-password"
	echo "FORECREW_GITHUB_PRIVATE_KEY=\"$key\""
	echo "FORECREW_GITHUB_WEBHOOK_SECRET=control-test-secret"
	echo "ANTHROPIC_API_KEY=control-test-anthropic"
	echo "CODEX_API_KEY=control-test-codex"
	echo "FORECREW_EVIDENCE_ACCESS_KEY_ID=GK$(openssl rand -hex 12)"
	echo "FORECREW_EVIDENCE_SECRET_ACCESS_KEY=$(openssl rand -hex 32)"
	echo "FORECREW_HTTP_PORT=$FORECREW_HTTP_PORT"
	echo "FORECREW_HTTPS_PORT=$FORECREW_HTTPS_PORT"
	echo "FORECREW_CONTROL_MAX_JOBS=2"
} >.env
{
	echo "GARAGE_RPC_SECRET=$(openssl rand -hex 32)"
	echo "GARAGE_ADMIN_TOKEN=$(openssl rand -hex 32)"
} >garage.env
cp forecrew.example.yaml forecrew.yaml

docker compose up -d --build --wait --wait-timeout 300 forecrewd worker-control
[ "$(docker compose ps -a --format '{{.Service}} {{.ExitCode}}' control-init)" = "control-init 0" ] ||
	fail "the init step did not exit 0"

# The worker registers and polls; its row says the pool, its labels and when it
# last polled.
row=""
for _ in $(seq 1 45); do
	row="$(docker compose exec -T postgres psql -U forecrew -d forecrew -tAc \
		"select pool || '|' || labels::text || '|' || case when last_seen_at > now() - interval '1 minute' then 'online' else 'offline' end
		 from workers where name = 'control' and revoked_at is null" 2>/dev/null || true)"
	case "$row" in *'|online') break ;; esac
	sleep 2
done
echo "worker row: $row"
case "$row" in
control\|*runtime:claude-code*runtime:codex*\|online) ;;
*) fail "control worker not online in pool control with runtime labels: $row" ;;
esac

# The worker reaches forecrewd's public API and nothing on the database network.
ip="$(docker inspect -f '{{with index .NetworkSettings.Networks "'"$COMPOSE_PROJECT_NAME"'_db"}}{{.IPAddress}}{{end}}' \
	"$(docker compose ps -q postgres)")"
[ -n "$ip" ] || fail "postgres has no address on the db network"
probe() { docker compose exec -T worker-control timeout 4 bash -c "</dev/tcp/$1/$2" 2>/dev/null; }
probe forecrewd 80 || fail "the control worker cannot reach forecrewd:80"
! probe postgres 5432 || fail "the control worker reaches postgres by name"
! probe "$ip" 5432 || fail "the control worker reaches postgres at $ip:5432"
# Nor the artifact store: workers upload through forecrewd.
! probe garage 3900 || fail "the control worker reaches garage:3900 (S3)"
! probe garage 3903 || fail "the control worker reaches garage:3903 (admin API)"
probe forecrewd 80 || fail "the control worker lost forecrewd:80"

# The agents run as forecrew and must not read the worker token.
! docker compose exec -T --user 10001 worker-control cat /var/lib/forecrew-control/token >/dev/null 2>&1 ||
	fail "the agents' user can read the worker token"

# PID 1 is the worker (control.sh execs it). It must run as forecrew with no
# new privileges and no effective capabilities; the bounding set holds only
# the two that control.sh uses to drop privileges, so cap_drop still applies.
status="$(docker compose exec -T worker-control cat /proc/1/status)"
proc() { awk -v k="$1:" '$1 == k { $1 = ""; print }' <<<"$status" | xargs; }
[ "$(proc Uid)" = "10001 10001 10001 10001" ] || fail "the worker runs as uid \"$(proc Uid)\", want 10001"
[ "$(proc NoNewPrivs)" = 1 ] || fail "the worker has no_new_privs $(proc NoNewPrivs), want 1"
[ "$(proc CapEff)" = 0000000000000000 ] || fail "the worker has effective capabilities $(proc CapEff)"
[ "$(proc CapBnd)" = 00000000000000c0 ] || fail "the worker bounding set is $(proc CapBnd), want SETUID and SETGID only"
# The worker's memory and environment stay private from the agents' user.
! docker compose exec -T --user 10001 worker-control cat /proc/1/environ >/dev/null 2>&1 ||
	fail "the agents' user can read the worker's environment"

# The worker gets no database, GitHub or runtime keys: forecrewd sends runtime
# keys with each job. The container's configured environment is what the
# worker process inherits (plus the token and HOME).
container="$(docker compose ps -q worker-control)"
names="$(docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$container" | cut -d= -f1)"
if grep -E '^(POSTGRES_|ANTHROPIC_API_KEY$|CODEX_API_KEY$|FORECREW_GITHUB_|FORECREW_DATABASE_URL$|GARAGE_)' <<<"$names"; then
	fail "the control worker has secrets in its environment (above)"
fi
grep -qx 'max_jobs: 2' <(docker compose exec -T worker-control cat /etc/forecrew-worker/worker.yaml) ||
	fail "FORECREW_CONTROL_MAX_JOBS did not reach worker.yaml"

# Running the init step again keeps the registration and the token.
docker compose run --rm -T control-init init | grep -q 'keeping its token' ||
	fail "the init step is not idempotent"
count="$(docker compose exec -T postgres psql -U forecrew -d forecrew -tAc \
	"select count(*) from workers where name = 'control'")"
[ "$count" = 1 ] || fail "worker control registered $count times"

echo "control worker tests passed"
