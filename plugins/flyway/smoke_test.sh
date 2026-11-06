#!/usr/bin/env bash
# Real smoke for the flyway plugin — builds the PINNED IMAGE and runs it against
# a throwaway PostgreSQL, so the contract is proven against a real database (not
# a stub) BEFORE the publishing build:
#   - migrate applies versioned SQL and the change actually lands in the DB;
#   - info + validate succeed against that applied history;
#   - PLUGIN_WORKING_DIR + relative locations resolve;
#   - connection comes from FLYWAY_* env and never appears on argv / in the log;
#   - the Postgres lock/statement-timeout initSql default is injected, and the
#     non-Postgres gate fails closed when those inputs are set off-Postgres.
# Convention-named so plugins.yml runs it. Needs docker + network (pulls postgres).
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMG="gocdnext-plugin-flyway:smoke-$$"
NET="flyway-smoke-$$"
PG="flyway-smoke-pg-$$"
WORK="$(mktemp -d)"
cleanup() {
  docker rm -f "$PG" >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
  docker image rm -f "$IMG" >/dev/null 2>&1 || true
  rm -rf "$WORK" || true
}
trap cleanup EXIT

die() { echo "FAIL: $1"; [ -n "${2:-}" ] && { echo "--- output ---"; echo "$2"; }; exit 1; }

echo "==> build (same Dockerfile/context as the official build)"
docker build -t "$IMG" "$HERE" >/dev/null

echo "==> start throwaway postgres"
docker network create "$NET" >/dev/null
docker run -d --name "$PG" --network "$NET" \
  -e POSTGRES_PASSWORD=secret -e POSTGRES_DB=app postgres:16-alpine >/dev/null
for _ in $(seq 1 40); do docker exec "$PG" pg_isready -U postgres >/dev/null 2>&1 && break; sleep 1; done
docker exec "$PG" pg_isready -U postgres >/dev/null 2>&1 || die "postgres did not become ready"

MIG="$WORK/migrations"; mkdir -p "$MIG"
printf 'CREATE TABLE widget (id INT PRIMARY KEY, name TEXT);\n' > "$MIG/V1__widget.sql"
printf 'ALTER TABLE widget ADD COLUMN qty INT DEFAULT 0;\n'      > "$MIG/V2__widget_qty.sql"

URL="jdbc:postgresql://$PG:5432"
# plug <command> <db> [extra docker args...] -> OUT, RC (never trips set -e)
plug() {
  local cmd="$1" db="$2"; shift 2
  set +e
  OUT="$(docker run --rm --network "$NET" -v "$MIG:/workspace/migrations" -w /workspace \
      -e FLYWAY_URL="$URL/$db" -e FLYWAY_USER=postgres -e FLYWAY_PASSWORD=secret \
      -e PLUGIN_COMMAND="$cmd" "$@" "$IMG" 2>&1)"; RC=$?
  set -e
}
q() { docker exec "$PG" psql -U postgres -d "$1" -tAc "$2" 2>/dev/null | tr -d '[:space:]'; }

# 1. migrate applies V1+V2 and the change lands in the DB.
plug migrate app
[ "$RC" -eq 0 ] || die "migrate should succeed (rc=$RC)" "$OUT"
[ "$(q app "SELECT count(*) FROM information_schema.columns WHERE table_name='widget' AND column_name='qty'")" = "1" ] \
  || die "V2 change (widget.qty) not applied to the database"
[ "$(q app "SELECT count(*) FROM flyway_schema_history WHERE success")" = "2" ] \
  || die "schema history should record 2 successful migrations"

# 2. info + validate succeed against the applied history.
plug info app;     [ "$RC" -eq 0 ] || die "info should succeed (rc=$RC)" "$OUT"
plug validate app; [ "$RC" -eq 0 ] || die "validate should succeed (rc=$RC)" "$OUT"

# 3. connection stays in env, never on argv / in the log.
printf '%s' "$OUT" | grep -q "secret" && die "credential leaked into the plugin output"
printf '%s' "$OUT" | grep -q '==> flyway validate (locations=' || die "unexpected command banner"

# 4. Postgres lock/statement-timeout initSql default is injected.
printf '%s' "$OUT" | grep -q "initSql: SET lock_timeout = '5s'" \
  || die "default Postgres lock/statement timeout initSql not injected" "$OUT"

# 5. PLUGIN_WORKING_DIR + relative locations resolve (migrate a fresh db via a nested dir).
docker exec "$PG" psql -U postgres -c "CREATE DATABASE app_wd" >/dev/null 2>&1
set +e
OUT="$(docker run --rm --network "$NET" -v "$MIG:/work/sub/migrations" -w /workspace \
    -e FLYWAY_URL="$URL/app_wd" -e FLYWAY_USER=postgres -e FLYWAY_PASSWORD=secret \
    -e PLUGIN_WORKING_DIR=/work/sub -e PLUGIN_COMMAND=migrate "$IMG" 2>&1)"; RC=$?
set -e
[ "$RC" -eq 0 ] || die "working-dir migrate should succeed (rc=$RC)" "$OUT"
[ "$(q app_wd "SELECT count(*) FROM flyway_schema_history WHERE success")" = "2" ] \
  || die "working-dir migrate did not apply via /work/sub"

# 6. non-Postgres gate: explicit lock-timeout off-Postgres fails closed (initSql is PG-only).
set +e
OUT="$(docker run --rm --network "$NET" -w /workspace \
    -e FLYWAY_URL="jdbc:mysql://nohost:3306/db" -e FLYWAY_USER=u -e FLYWAY_PASSWORD=p \
    -e PLUGIN_COMMAND=info -e PLUGIN_LOCK_TIMEOUT=5s "$IMG" 2>&1)"; RC=$?
set -e
{ [ "$RC" -eq 2 ] && printf '%s' "$OUT" | grep -q "Postgres-only"; } \
  || die "non-Postgres URL with explicit lock-timeout must exit 2 (rc=$RC)" "$OUT"

echo "PASS: flyway smoke"
