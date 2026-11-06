#!/usr/bin/env bash
# Mock-PATH test for the flyway entrypoint — the guard/argv logic that doesn't
# need a real database (the DB-backed contract is in smoke_test.sh). A mock
# `flyway` records its argv + the injected FLYWAY_INIT_SQL; the entrypoint must
# reject bad/empty/repair commands and a missing FLYWAY_URL BEFORE calling it.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ENTRY="$HERE/entrypoint.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

cat >"$TMP/flyway" <<'EOF'
#!/usr/bin/env bash
{ echo "ARGS=$*"; echo "INIT_SQL=${FLYWAY_INIT_SQL:-<unset>}"; } >> "$LOG"
exit 0
EOF
chmod +x "$TMP/flyway"

fails=0
PG="jdbc:postgresql://db:5432/app"
run() { local log="$1"; shift; : >"$log"; ( env "$@" LOG="$log" PATH="$TMP:$PATH" sh "$ENTRY" >"$log.out" 2>&1 ); echo $?; }
called() { [ -s "$1" ]; }   # mock wrote to the LOG => flyway was invoked

# 1. invalid / empty / repair commands exit 2 and never call flyway.
for c in "" "foo" "repair"; do
  rc=$(run "$TMP/1.log" PLUGIN_COMMAND="$c" FLYWAY_URL="$PG")
  [ "$rc" = 2 ] || { echo "FAIL: command '$c' should exit 2 (got $rc)"; fails=$((fails+1)); }
  called "$TMP/1.log" && { echo "FAIL: command '$c' must not call flyway"; fails=$((fails+1)); }
done

# 2. missing FLYWAY_URL exits 2, flyway not called.
rc=$(run "$TMP/2.log" PLUGIN_COMMAND=migrate)
[ "$rc" = 2 ] || { echo "FAIL: missing FLYWAY_URL should exit 2 (got $rc)"; fails=$((fails+1)); }
called "$TMP/2.log" && { echo "FAIL: missing FLYWAY_URL must not call flyway"; fails=$((fails+1)); }

# 3. valid migrate on Postgres: flyway gets -locations + command, and the
#    lock/statement timeout initSql default is injected.
rc=$(run "$TMP/3.log" PLUGIN_COMMAND=migrate FLYWAY_URL="$PG")
[ "$rc" = 0 ] || { echo "FAIL: valid migrate should exit 0 (got $rc)"; cat "$TMP/3.log.out"; fails=$((fails+1)); }
grep -q "ARGS=-locations=filesystem:./migrations migrate" "$TMP/3.log" || { echo "FAIL: flyway argv (locations+command)"; cat "$TMP/3.log"; fails=$((fails+1)); }
grep -q "INIT_SQL=SET lock_timeout = '5s'; SET statement_timeout = '15min';" "$TMP/3.log" || { echo "FAIL: PG initSql default not injected"; cat "$TMP/3.log"; fails=$((fails+1)); }

# 4. PLUGIN_LOCATIONS override flows onto argv.
rc=$(run "$TMP/4.log" PLUGIN_COMMAND=info FLYWAY_URL="$PG" PLUGIN_LOCATIONS="filesystem:./db/migs")
grep -q "ARGS=-locations=filesystem:./db/migs info" "$TMP/4.log" || { echo "FAIL: locations override not honored"; cat "$TMP/4.log"; fails=$((fails+1)); }

# 5. non-Postgres + explicit lock-timeout fails closed (exit 2, flyway not called).
rc=$(run "$TMP/5.log" PLUGIN_COMMAND=info FLYWAY_URL="jdbc:mysql://h/db" PLUGIN_LOCK_TIMEOUT=5s)
[ "$rc" = 2 ] || { echo "FAIL: non-PG + lock-timeout should exit 2 (got $rc)"; fails=$((fails+1)); }
called "$TMP/5.log" && { echo "FAIL: non-PG gate must not call flyway"; fails=$((fails+1)); }

# 6. non-Postgres without timeout inputs: flyway runs, no PG initSql injected.
rc=$(run "$TMP/6.log" PLUGIN_COMMAND=info FLYWAY_URL="jdbc:mysql://h/db")
[ "$rc" = 0 ] || { echo "FAIL: non-PG info should exit 0 (got $rc)"; cat "$TMP/6.log.out"; fails=$((fails+1)); }
grep -q "INIT_SQL=<unset>" "$TMP/6.log" || { echo "FAIL: non-PG must not inject PG initSql"; cat "$TMP/6.log"; fails=$((fails+1)); }

if [ "$fails" -eq 0 ]; then echo "PASS: flyway entrypoint"; else echo "FAILED: $fails check(s)"; exit 1; fi
