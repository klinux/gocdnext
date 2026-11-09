#!/usr/bin/env bash
# Real smoke for the python plugin's IMAGE. Builds the pinned image and runs it
# end to end so the uv 0.5.5 -> 0.13.7 bump (GHSA-hq85-5cpj-mr62) fails HERE,
# before publish. Proves:
#   - uv is 0.13.7 (the CVE floor is 0.13.4), poetry/python still present;
#   - uv's stable surface the entrypoint relies on still WORKS across the big
#     jump: `uv sync` (no lockfile) resolves + installs a real dep and `uv run`
#     executes against it; `uv sync --frozen` honours a committed uv.lock. A
#     behaviour/flag regression in uv would throw here, not in production.
#   - a FAILING user command still propagates a non-zero exit (no false green).
#
# Scoped to the uv manager (what this bump touches); the pip/poetry managers are
# unaffected by the uv version and are not re-tested here.
#
# The real install needs the package index (like a real job's install step), so
# these run with network; the failing case is offline. Built --no-cache to match
# a release build. Workspace outputs are root-owned -> cleaned in-image;
# fail-closed.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMG="gocdnext-plugin-python:smoke-$$"
CNAME="pysmoke-$$"
WOK="$(mktemp -d)"   # uv (no lockfile) project
WLK="$(mktemp -d)"   # uv (--frozen, committed lock) project
WBAD="$(mktemp -d)"  # failing-command project
SMOKE_OK=0

cleanup() {
  docker rm -f "$CNAME" >/dev/null 2>&1 || true
  if docker image inspect "$IMG" >/dev/null 2>&1; then
    docker run --rm --entrypoint bash -v "$WOK:/a" -v "$WLK:/b" -v "$WBAD:/d" "$IMG" \
      -c 'for x in /a /b /d; do rm -rf "$x"/* "$x"/.[!.]* "$x"/..?* 2>/dev/null || true; done' \
      >/dev/null 2>&1 || true
  fi
  docker image rm -f "$IMG" >/dev/null 2>&1 || true
  rm -rf "$WOK" "$WLK" "$WBAD" 2>/dev/null || true
  leftover=0
  for d in "$WOK" "$WLK" "$WBAD"; do
    [ -d "$d" ] && { echo "WARN: cleanup could not fully remove ${d}" >&2; leftover=1; }
  done
  docker image inspect "$IMG" >/dev/null 2>&1 && { echo "WARN: smoke image not removed" >&2; leftover=1; }
  [ "$leftover" = 1 ] && [ "$SMOKE_OK" = 1 ] && { echo "FAIL: smoke passed but left state behind" >&2; exit 1; }
  return 0
}
trap cleanup EXIT
die() { echo "FAIL: $1"; [ -n "${2:-}" ] && { echo "--- output ---"; echo "$2" | tail -40; }; exit 1; }

# Hard wall-clock cap with TERM->KILL escalation (RC=124 TERM, RC=137 KILL).
run() { set +e; OUT="$(timeout --kill-after=15s 300s "$@" 2>&1)"; RC=$?; set -e; }

# A minimal project that depends on a tiny, stable, pure-Python package
# (iniconfig) so `uv sync` / pip must actually resolve+install something, and the
# user command imports it to prove the install landed in the active environment.
# App-style (not a library): `package = false` tells uv to install the
# dependencies into .venv without trying to BUILD the root project, so no module
# layout / build backend is needed for the smoke.
PROJECT_TOML='[project]
name = "smoke"
version = "0.0.0"
requires-python = ">=3.12"
dependencies = ["iniconfig==2.0.0"]

[tool.uv]
package = false'

echo "==> build plugin image (--no-cache, same Dockerfile/context as the official build)"
docker build --no-cache -t "$IMG" "$HERE" >/dev/null

echo "==> uv is 0.13.7 and poetry/python are present"
# Probe each tool in its OWN container: a single `sh -c 'a; b; c'` returns only
# c's status, masking a failure of a or b, and a loose substring would match an
# error line ("poetry: not found" contains "poetry"). Require exit 0 AND a strict
# version line per tool.
check_tool() {
  local label="$1" bin="$2" flag="$3" pat="$4" out rc
  set +e
  out="$(docker run --rm --network none --entrypoint "$bin" "$IMG" "$flag" 2>&1)"; rc=$?
  set -e
  [ "$rc" -eq 0 ] || die "${label}: '${bin} ${flag}' exited ${rc}" "$out"
  grep -qE "$pat" <<<"$out" || die "${label}: version line /${pat}/ not found" "$out"
}
check_tool "uv"     uv     --version 'uv 0\.13\.7'
check_tool "poetry" poetry --version 'Poetry \(version 1\.8\.4\)'
check_tool "python" python --version 'Python 3\.12'

echo "==> uv manager (no lockfile): 'uv sync' installs a real dep; user command imports it"
printf '%s\n' "$PROJECT_TOML" > "$WOK/pyproject.toml"
run docker run --rm --name "$CNAME" -v "$WOK:/workspace" -w /workspace \
  -e PLUGIN_MANAGER=uv -e PLUGIN_COMMAND='uv run python -c "import iniconfig; print(\"UV_OK\", iniconfig.__name__)"' "$IMG"
[ "$RC" -eq 0 ] || die "uv (no lock): expected exit 0, got $RC" "$OUT"
grep -q 'UV_OK iniconfig' <<<"$OUT" || die "uv (no lock): dep not importable after sync" "$OUT"

echo "==> uv manager (--frozen): the committed uv.lock is used verbatim, stale pyproject ignored"
# Lock with iniconfig only (image's own uv, under the watchdog + named container).
printf '%s\n' "$PROJECT_TOML" > "$WLK/pyproject.toml"
run docker run --rm --name "$CNAME" -v "$WLK:/workspace" -w /workspace --entrypoint uv "$IMG" lock
[ "$RC" -eq 0 ] || die "uv lock: expected exit 0, got $RC" "$OUT"
[ -f "$WLK/uv.lock" ] || die "uv.lock was not created" "$OUT"
LOCK_BEFORE="$(sha256sum "$WLK/uv.lock" | awk '{print $1}')"
# Now make pyproject deliberately STALE: add a dependency NOT in the lock. A plain
# `uv sync` would re-resolve and install it + rewrite the lock; `uv sync --frozen`
# must use the lock verbatim — install only iniconfig, never touch the lock.
printf '%s\n' "$PROJECT_TOML" | sed 's/"iniconfig==2.0.0"/"iniconfig==2.0.0", "packaging==24.2"/' > "$WLK/pyproject.toml"
run docker run --rm --name "$CNAME" -v "$WLK:/workspace" -w /workspace \
  -e PLUGIN_MANAGER=uv \
  -e PLUGIN_COMMAND='.venv/bin/python -c "import iniconfig; import importlib.util as u; assert u.find_spec(\"packaging\") is None, \"FROZEN_LEAK: stale dep was installed\"; print(\"UV_FROZEN_OK\")"' "$IMG"
[ "$RC" -eq 0 ] || die "uv (--frozen): expected exit 0, got $RC" "$OUT"
grep -q 'UV_FROZEN_OK' <<<"$OUT" || die "uv (--frozen): frozen env missing iniconfig or leaked the stale dep" "$OUT"
LOCK_AFTER="$(sha256sum "$WLK/uv.lock" | awk '{print $1}')"
[ "$LOCK_BEFORE" = "$LOCK_AFTER" ] || die "uv (--frozen): the lockfile changed — --frozen re-resolved instead of using it verbatim" "$OUT"

echo "==> a FAILING user command propagates a non-zero exit (no false green, offline)"
printf '%s\n' "$PROJECT_TOML" > "$WBAD/pyproject.toml"
run docker run --rm --name "$CNAME" --network none -v "$WBAD:/workspace" -w /workspace \
  -e PLUGIN_MANAGER=none -e PLUGIN_COMMAND='python -c "import sys; sys.exit(7)"' "$IMG"
case "$RC" in
  124) die "failing case: watchdog SIGTERM at 300s — entrypoint hung" "$OUT" ;;
  137) die "failing case: watchdog SIGKILL after 300s+15s — entrypoint hung" "$OUT" ;;
esac
[ "$RC" -eq 7 ] || die "failing case: expected the command's exit 7 to propagate, got $RC" "$OUT"

SMOKE_OK=1
echo "PASS: python smoke"
