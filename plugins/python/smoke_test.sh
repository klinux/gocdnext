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
TOOLS="$(docker run --rm --network none --entrypoint sh "$IMG" -c 'uv --version; poetry --version; python --version')" \
  || die "tool version probe failed" "$TOOLS"
grep -qE '^uv 0\.13\.7( |$)' <<<"$TOOLS" || die "uv is not 0.13.7" "$TOOLS"
grep -qiE 'poetry'           <<<"$TOOLS" || die "poetry missing" "$TOOLS"
grep -qE 'Python 3\.12'      <<<"$TOOLS" || die "python 3.12 missing" "$TOOLS"

echo "==> uv manager (no lockfile): 'uv sync' installs a real dep; user command imports it"
printf '%s\n' "$PROJECT_TOML" > "$WOK/pyproject.toml"
run docker run --rm --name "$CNAME" -v "$WOK:/workspace" -w /workspace \
  -e PLUGIN_MANAGER=uv -e PLUGIN_COMMAND='uv run python -c "import iniconfig; print(\"UV_OK\", iniconfig.__name__)"' "$IMG"
[ "$RC" -eq 0 ] || die "uv (no lock): expected exit 0, got $RC" "$OUT"
grep -q 'UV_OK iniconfig' <<<"$OUT" || die "uv (no lock): dep not importable after sync" "$OUT"

echo "==> uv manager (--frozen): a committed uv.lock is honoured"
cp "$WOK/pyproject.toml" "$WLK/pyproject.toml"
# Generate the lockfile with the image's own uv so it matches the resolver, then
# run with it present -> the entrypoint takes the `uv sync --frozen` branch.
docker run --rm -v "$WLK:/workspace" -w /workspace --entrypoint uv "$IMG" lock >/dev/null 2>&1 \
  || die "uv lock failed to produce a lockfile"
[ -f "$WLK/uv.lock" ] || die "uv.lock was not created"
run docker run --rm --name "$CNAME" -v "$WLK:/workspace" -w /workspace \
  -e PLUGIN_MANAGER=uv -e PLUGIN_COMMAND='uv run python -c "import iniconfig; print(\"UV_FROZEN_OK\")"' "$IMG"
[ "$RC" -eq 0 ] || die "uv (--frozen): expected exit 0, got $RC" "$OUT"
grep -q 'UV_FROZEN_OK' <<<"$OUT" || die "uv (--frozen): frozen sync did not yield a working env" "$OUT"

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
