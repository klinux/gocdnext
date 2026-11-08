#!/usr/bin/env bash
# Real smoke for the goose plugin's IMAGE. Runs the PINNED binary end to end,
# fully OFFLINE (--network none). Proves the bump (goose v3.27.0 -> v3.29.0 on
# go1.27, alpine 3.24.4):
#   - the binary is v3.29.0;
#   - the entrypoint guards hold: missing command, a non-whitelisted command,
#     down/redo (forward-only), missing GOOSE_DBSTRING, missing dir all exit 2;
#   - `validate` passes on a well-formed migration and fails on a malformed one;
#   - the CORE flow runs against a real driver: `up` applies a migration to a
#     SQLite DB in the workspace and `status` then reports it as applied.
#
# Convention-named so plugins.yml runs it as a PRE-PUBLISH gate. Needs docker.
# Built --no-cache to match the no_cache publish build. Trap cleanup, fail-closed.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMG="gocdnext-plugin-goose:smoke-$$"
W="$(mktemp -d)"
SMOKE_OK=0
cleanup() {
  # `goose up` writes a root-owned SQLite DB into the mount; remove it from inside
  # the image (matching uid) before deleting the dir, then verify it's gone.
  if docker image inspect "$IMG" >/dev/null 2>&1; then
    docker run --rm --entrypoint sh -v "$W:/w" "$IMG" \
      -c 'rm -rf /w/* /w/.[!.]* /w/..?* 2>/dev/null || true' >/dev/null 2>&1 || true
  fi
  docker image rm -f "$IMG" >/dev/null 2>&1 || true
  rm -rf "$W" 2>/dev/null || true
  leftover=0
  [ -d "$W" ] && { echo "WARN: cleanup could not remove ${W}" >&2; leftover=1; }
  docker image inspect "$IMG" >/dev/null 2>&1 && { echo "WARN: smoke image not removed" >&2; leftover=1; }
  [ "$leftover" = 1 ] && [ "$SMOKE_OK" = 1 ] && { echo "FAIL: smoke passed but left state behind" >&2; exit 1; }
  return 0
}
trap cleanup EXIT
die() { echo "FAIL: $1"; [ -n "${2:-}" ] && { echo "--- output ---"; echo "$2" | tail -40; }; exit 1; }
# DSN is a dummy — validate never connects; the guards run before goose anyway.
DSN="postgres://u:p@db:5432/app"
run() { # run <extra -e...> : sets RC+OUT, offline
  set +e
  OUT="$(docker run --rm --network none -v "$W:/workspace" -w /workspace "$@" "$IMG" 2>&1)"; RC=$?
  set -e
}

echo "==> build plugin image (--no-cache, same Dockerfile/context as the official build)"
docker build --no-cache -t "$IMG" "$HERE" >/dev/null

echo "==> goose is v3.29.0"
OUT="$(docker run --rm --network none --entrypoint goose "$IMG" --version 2>&1)" || die "goose --version failed" "$OUT"
grep -q 'v3.29.0' <<<"$OUT" || die "goose is not v3.29.0 (got: ${OUT})" "$OUT"

mkdir -p "$W/migrations"
cat >"$W/migrations/00001_init.sql" <<'SQL'
-- +goose Up
CREATE TABLE smoke (id int);
-- +goose Down
DROP TABLE smoke;
SQL

echo "==> guards: missing/non-whitelisted command, down/redo (forward-only), missing DSN, missing dir -> exit 2"
run -e PLUGIN_COMMAND="" -e GOOSE_DBSTRING="$DSN" -e PLUGIN_DIR=migrations
[ "$RC" -eq 2 ] || die "missing-command: expected exit 2, got $RC" "$OUT"
run -e PLUGIN_COMMAND="create" -e GOOSE_DBSTRING="$DSN" -e PLUGIN_DIR=migrations   # not whitelisted
[ "$RC" -eq 2 ] || die "non-whitelisted command: expected exit 2, got $RC" "$OUT"
for fw in down redo; do
  run -e PLUGIN_COMMAND="$fw" -e GOOSE_DBSTRING="$DSN" -e PLUGIN_DIR=migrations
  [ "$RC" -eq 2 ] || die "$fw: expected exit 2 (forward-only), got $RC" "$OUT"
  grep -qi 'forward-only' <<<"$OUT" || die "$fw: missing the forward-only message" "$OUT"
done
run -e PLUGIN_COMMAND="validate" -e PLUGIN_DIR=migrations
[ "$RC" -eq 2 ] || die "missing-DSN: expected exit 2, got $RC" "$OUT"
run -e PLUGIN_COMMAND="validate" -e GOOSE_DBSTRING="$DSN" -e PLUGIN_DIR="nope"
[ "$RC" -eq 2 ] || die "missing-dir: expected exit 2, got $RC" "$OUT"

echo "==> validate passes on a well-formed migration (offline)"
run -e PLUGIN_COMMAND="validate" -e GOOSE_DBSTRING="$DSN" -e PLUGIN_DIR=migrations
[ "$RC" -eq 0 ] || die "validate-good: expected exit 0, got $RC" "$OUT"

echo "==> validate fails on a malformed migration"
printf 'CREATE TABLE no_annotations (id int);\n' >"$W/migrations/00002_bad.sql"
run -e PLUGIN_COMMAND="validate" -e GOOSE_DBSTRING="$DSN" -e PLUGIN_DIR=migrations
[ "$RC" -ne 0 ] || die "validate-bad: expected a non-zero exit, got 0" "$OUT"
grep -qi 'failed to parse' <<<"$OUT" || die "validate-bad: goose did not report the parse error" "$OUT"
rm -f "$W/migrations/00002_bad.sql"   # keep the up/status flow on the valid set

echo "==> up applies a migration to a SQLite DB, then status reports it applied (offline)"
SQLITE_DSN="./goose-smoke.db"
run -e PLUGIN_COMMAND="up" -e PLUGIN_DRIVER="sqlite3" -e GOOSE_DBSTRING="$SQLITE_DSN" -e PLUGIN_DIR=migrations
[ "$RC" -eq 0 ] || die "up: expected exit 0, got $RC" "$OUT"
grep -qi 'successfully migrated database to version: 1' <<<"$OUT" \
  || die "up: goose did not apply the migration (driver regression?)" "$OUT"
run -e PLUGIN_COMMAND="status" -e PLUGIN_DRIVER="sqlite3" -e GOOSE_DBSTRING="$SQLITE_DSN" -e PLUGIN_DIR=migrations
[ "$RC" -eq 0 ] || die "status: expected exit 0, got $RC" "$OUT"
grep -q '00001_init.sql' <<<"$OUT" || die "status: migration not listed" "$OUT"
grep -qi 'Pending' <<<"$OUT" && die "status: migration still Pending after up (not applied)" "$OUT" || true

SMOKE_OK=1
echo "PASS: goose smoke"
