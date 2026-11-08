#!/usr/bin/env bash
# Real smoke for the goose plugin's IMAGE. Runs the PINNED binary end to end,
# fully OFFLINE (--network none) — `validate` parses migration files without a DB.
# Proves the bump (goose v3.27.0 -> v3.29.0 on go1.27, alpine 3.24.4):
#   - the binary is v3.29.0;
#   - the entrypoint guards hold: missing/!whitelisted command, down/redo
#     (forward-only), missing GOOSE_DBSTRING, missing dir all exit 2;
#   - `validate` passes on a well-formed migration and fails on a malformed one.
#
# Convention-named so plugins.yml runs it as a PRE-PUBLISH gate. Needs docker.
# Built --no-cache to match the no_cache publish build. Trap cleanup, fail-closed.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMG="gocdnext-plugin-goose:smoke-$$"
W="$(mktemp -d)"
SMOKE_OK=0
cleanup() {
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

echo "==> guards: missing command, down (forward-only), missing DSN, missing dir -> exit 2"
run -e PLUGIN_COMMAND="" -e GOOSE_DBSTRING="$DSN" -e PLUGIN_DIR=migrations
[ "$RC" -eq 2 ] || die "missing-command: expected exit 2, got $RC" "$OUT"
run -e PLUGIN_COMMAND="down" -e GOOSE_DBSTRING="$DSN" -e PLUGIN_DIR=migrations
[ "$RC" -eq 2 ] || die "down: expected exit 2 (forward-only), got $RC" "$OUT"
grep -qi 'forward-only' <<<"$OUT" || die "down: missing the forward-only message" "$OUT"
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

SMOKE_OK=1
echo "PASS: goose smoke"
