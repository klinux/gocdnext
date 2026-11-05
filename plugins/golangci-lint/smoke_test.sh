#!/usr/bin/env bash
# Real smoke for the golangci-lint plugin — builds and runs the PINNED IMAGE
# (not the entrypoint mock), so the things a stub can't prove are gated BEFORE
# the publishing build:
#   - the bundled golangci-lint is built with a Go >= the module's `go`
#     directive, i.e. it actually lints a Go 1.26 module instead of refusing it
#     ("lower than the targeted Go version" — the v1.63 / Go 1.23 regression
#     the upgrade to golangci-lint v2.15.0 on Go 1.27 fixes);
#   - the entrypoint's absolute-cache fix lets the internal `go list` run
#     end-to-end instead of dying on a relative GOMODCACHE/GOCACHE;
#   - `golangci-lint run --timeout` is still a valid flag under v2;
#   - a real issue is actually reported with a non-zero exit, not no-op'd.
# Convention-named so plugins.yml runs it. Hermetic: fixtures use stdlib only.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMG="gocdnext-plugin-golangci-lint:smoke-$$"
WORK="$(mktemp -d)"
cleanup() {
  # Fixtures run as the host user (see plug), so the caches golangci-lint wrote
  # into the mounts are host-owned and a plain rm removes them. No `|| true`
  # mask: if cleanup ever fails it's a real leak (e.g. root-owned files) and the
  # smoke should fail loudly rather than hide 30+ MB of orphaned cache.
  rm -rf "${WORK}"
  docker image rm -f "${IMG}" >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "==> build (same Dockerfile/context as the official build)"
docker build -t "${IMG}" "${HERE}" >/dev/null

# plug <fixture-dir> [docker-run-flags...] — run the plugin image against a
# fixture, capturing output + exit code WITHOUT tripping `set -e` (a failing
# lint is the expected outcome for the "bad" fixture). Never `plug | grep`: grep
# closes the pipe on first match, the still-writing `docker run` takes SIGPIPE,
# and pipefail turns that into a racy false failure (see the node smoke).
plug() {
  local dir="$1"; shift
  set +e
  # --user + HOME under the mount: the plugin needs no root, and running as the
  # host user keeps the caches it writes host-owned so cleanup can remove them
  # (the image otherwise runs as root and leaves root-owned cache behind).
  PLUG_OUT="$(docker run --rm --user "$(id -u):$(id -g)" -e HOME=/workspace \
    -v "${dir}:/workspace" -w /workspace "$@" "${IMG}" 2>&1)"
  PLUG_RC=$?
  set -e
}
die() { echo "FAIL: $1"; [ -n "${PLUG_OUT:-}" ] && { echo "--- output ---"; echo "$PLUG_OUT"; }; exit 1; }

mkmod() { # mkmod <name> <go-file-body>  -> prints the module dir
  local d="${WORK}/$1"; mkdir -p "$d"
  printf 'module smoke\n\ngo 1.26\n' >"$d/go.mod"
  printf '%s' "$2" >"$d/x.go"
  echo "$d"
}

# 1. A clean Go 1.26 module lints green — proves the bundled golangci-lint is
#    built with Go >= 1.26 (v1.63 refused this) and the cache paths resolve.
clean="$(mkmod clean 'package smoke

// Hello returns a greeting.
func Hello() string { return "ok" }
')"
plug "$clean"
[ "$PLUG_RC" -eq 0 ] || die "clean Go 1.26 module should lint green (rc=$PLUG_RC)"
printf '%s\n' "$PLUG_OUT" | grep -q "0 issues" || die "expected '0 issues' on the clean module"
# Regression guard: the v1.63 blocker signature must NOT appear.
if printf '%s\n' "$PLUG_OUT" | grep -qi "lower than the targeted Go version"; then
  die "golangci-lint is older than the module's Go — image not built with Go >= 1.26"
fi

# 2. A real issue is reported with a non-zero exit (proves it actually lints,
#    not just no-ops). A govet printf mismatch is default-enabled under v2.
bad="$(mkmod bad 'package smoke

import "fmt"

// Bad has a govet printf mismatch (%d given a string).
func Bad() string { return fmt.Sprintf("%d", "not-a-number") }
')"
plug "$bad"
[ "$PLUG_RC" -ne 0 ] || die "a module with a govet issue should exit non-zero"
printf '%s\n' "$PLUG_OUT" | grep -qiE "govet|wrong type|Sprintf" || die "expected the govet printf issue to be reported"

# 3. PLUGIN_TIMEOUT override still passes through as a valid --timeout flag.
plug "$clean" -e PLUGIN_TIMEOUT=3m
[ "$PLUG_RC" -eq 0 ] || die "PLUGIN_TIMEOUT override should still lint green (rc=$PLUG_RC)"

echo "PASS: golangci-lint smoke"
