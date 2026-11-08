#!/usr/bin/env bash
# Real smoke for the liquibase plugin's IMAGE. Runs the PINNED binary end to end,
# fully OFFLINE (--network none) — liquibase's `offline:` JDBC URL generates SQL /
# validates without a database. Proves the hardening (alpine `apk upgrade` + the
# Liquibase Package Manager `lpm` removed) did NOT break liquibase:
#   - liquibase is 4.31.1 and `lpm` is gone (the removed CVE source);
#   - the entrypoint guards (missing/non-whitelisted command, rollback forward-only,
#     missing LIQUIBASE_COMMAND_URL, missing changelog) all exit 2;
#   - `validate` passes a well-formed changelog and fails a malformed one;
#   - `update-sql` generates the expected DDL offline.
#
# Convention-named so plugins.yml runs it as a PRE-PUBLISH gate. Needs docker.
# Built --no-cache to match the no_cache publish build. Trap cleanup, fail-closed.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMG="gocdnext-plugin-liquibase:smoke-$$"
W="$(mktemp -d)"
SMOKE_OK=0
cleanup() {
  # liquibase (offline) writes databasechangelog.csv into the mount as the
  # container user; remove it from inside the image before deleting the dir.
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
URL="offline:postgresql"
CL="db/changelog/db.changelog-master.yaml"
good_changelog() {
  mkdir -p "$W/db/changelog"
  cat >"$W/$CL" <<'YAML'
databaseChangeLog:
  - changeSet:
      id: 1
      author: smoke
      changes:
        - createTable:
            tableName: smoke
            columns:
              - column: {name: id, type: int}
YAML
  chmod -R a+rwX "$W"   # the container's 'liquibase' user reads + writes the mount
}
run() { set +e; OUT="$(docker run --rm --network none -v "$W:/workspace" -w /workspace "$@" "$IMG" 2>&1)"; RC=$?; set -e; }

echo "==> build plugin image (--no-cache, same Dockerfile/context as the official build)"
docker build --no-cache -t "$IMG" "$HERE" >/dev/null

echo "==> liquibase is 4.31.1 and lpm (the removed CVE source) is gone"
OUT="$(docker run --rm --network none --entrypoint liquibase "$IMG" --version 2>&1)" || die "liquibase --version failed" "$OUT"
grep -qi 'Liquibase Version: 4.31.1' <<<"$OUT" || die "liquibase is not 4.31.1 (got: $(grep -i version <<<"$OUT" | head -1))" "$OUT"
if docker run --rm --network none --entrypoint sh "$IMG" -c '[ -e /liquibase/bin/lpm ]' 2>/dev/null; then
  die "lpm is still present — the removed CVE source is back"
fi

echo "==> guards: missing/non-whitelisted command, rollback, missing URL, missing changelog -> exit 2"
good_changelog
run -e PLUGIN_COMMAND="" -e LIQUIBASE_COMMAND_URL="$URL"
[ "$RC" -eq 2 ] || die "missing-command: expected exit 2, got $RC" "$OUT"
run -e PLUGIN_COMMAND="snapshot" -e LIQUIBASE_COMMAND_URL="$URL"   # not whitelisted
[ "$RC" -eq 2 ] || die "non-whitelisted: expected exit 2, got $RC" "$OUT"
run -e PLUGIN_COMMAND="rollback" -e LIQUIBASE_COMMAND_URL="$URL"
[ "$RC" -eq 2 ] || die "rollback: expected exit 2 (forward-only), got $RC" "$OUT"
grep -qi 'forward-only' <<<"$OUT" || die "rollback: missing the forward-only message" "$OUT"
run -e PLUGIN_COMMAND="validate"
[ "$RC" -eq 2 ] || die "missing-URL: expected exit 2, got $RC" "$OUT"
run -e PLUGIN_COMMAND="validate" -e LIQUIBASE_COMMAND_URL="$URL" -e PLUGIN_CHANGELOG_FILE="nope.yaml"
[ "$RC" -eq 2 ] || die "missing-changelog: expected exit 2, got $RC" "$OUT"

echo "==> validate passes a well-formed changelog (offline)"
run -e PLUGIN_COMMAND="validate" -e LIQUIBASE_COMMAND_URL="$URL"
[ "$RC" -eq 0 ] || die "validate-good: expected exit 0, got $RC" "$OUT"
grep -qi 'No validation errors' <<<"$OUT" || die "validate-good: unexpected output" "$OUT"

echo "==> update-sql generates the expected DDL (offline)"
run -e PLUGIN_COMMAND="update-sql" -e LIQUIBASE_COMMAND_URL="$URL"
[ "$RC" -eq 0 ] || die "update-sql: expected exit 0, got $RC" "$OUT"
grep -qiE 'CREATE TABLE smoke' <<<"$OUT" || die "update-sql: did not generate the expected DDL" "$OUT"

echo "==> validate fails a malformed changelog"
printf 'databaseChangeLog: [ this is not: valid: liquibase\n' >"$W/$CL"   # dir already writable
run -e PLUGIN_COMMAND="validate" -e LIQUIBASE_COMMAND_URL="$URL"
[ "$RC" -ne 0 ] || die "validate-bad: expected a non-zero exit, got 0" "$OUT"

SMOKE_OK=1
echo "PASS: liquibase smoke"
