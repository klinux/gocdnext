#!/usr/bin/env bash
# Real smoke for the node plugin — runs the PINNED IMAGE (not the mock),
# so the things a stubbed PATH can't prove are gated BEFORE the publishing
# build: node availability inside `bash -lc` (the /etc/profile PATH reset),
# real corepack (incl. the no-packageManager path that errored), the baked
# bun, and the relocated-node symlinks (dangling yarn / nodejs alias).
# Convention-named so plugins.yml runs it. Needs network for corepack.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMG="gocdnext-plugin-node:smoke-$$"
WORK="$(mktemp -d)"
cleanup() {
  docker image rm -f "${IMG}" >/dev/null 2>&1 || true
  rm -rf "${WORK}" >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "==> build (same Dockerfile/context as the official build)"
docker build -t "${IMG}" "${HERE}" >/dev/null

# plug <fixture-dir> <env-flags...> — run the plugin against a fixture.
plug() {
  local dir="$1"; shift
  docker run --rm -v "${dir}:/workspace" -w /workspace "$@" "${IMG}" 2>&1
}
mkfix() { local d="${WORK}/$1"; mkdir -p "$d"; printf '%s' "$2" >"$d/package.json"; echo "$d"; }
die() { echo "FAIL: $1"; [ -n "${2:-}" ] && { echo "--- output ---"; echo "$2"; }; exit 1; }

# 1. node major selected from engines.node AND actually on PATH inside the
#    `bash -lc` the entrypoint execs (the /etc/profile-reset bug).
for maj in 20 22 24; do
  d="$(mkfix "n${maj}" "{\"engines\":{\"node\":\"${maj}.x\"}}")"
  out="$(plug "$d" -e PLUGIN_MANAGER=none -e PLUGIN_COMMAND='node --version')"
  echo "$out" | grep -qE "==> node: ${maj} " || die "engines ${maj}.x not selected" "$out"
  echo "$out" | grep -qE "^v${maj}\." || die "node v${maj} not on PATH in bash -lc" "$out"
done

# NOTE: always CAPTURE the plug output then grep the string — never
# `plug | grep -q`. grep -q closes the pipe on first match, the still-
# writing `docker run` takes SIGPIPE and exits 141, and `set -o pipefail`
# turns that into a (racy) false failure even though the match succeeded.

# 2. no declaration → default node 22.
out="$(plug "$(mkfix def '{}')" -e PLUGIN_MANAGER=none -e PLUGIN_COMMAND='node --version')"
printf '%s\n' "$out" | grep -qE '==> node: 22 \(default\)' || die "default node not 22" "$out"

# 2b. real semver eval (proves the bundled semver is found + used):
#     open range ">=18" → node 20; operator SPACE ">= 18" and a HYPHEN
#     range "18 - 22" are valid semver and must resolve too (the bash
#     parser used to mis-handle these).
for rng in '>=18' '>= 18' '18 - 22'; do
  out="$(plug "$(mkfix "ge_$$" "{\"engines\":{\"node\":\"${rng}\"}}")" -e PLUGIN_MANAGER=none -e PLUGIN_COMMAND='node --version')"
  printf '%s\n' "$out" | grep -qE '==> node: 20 ' || die "engines '${rng}' should widen to node 20" "$out"
  printf '%s\n' "$out" | grep -qE '^v20\.' || die "engines '${rng}' should actually run node 20" "$out"
done

# 2b-2. MINOR/PATCH honored: ">=20.99.0" is NOT met by baked 20 (20.20.2)
#       but IS met by 22 → must skip 20 and pick node 22 (the old
#       major-only parser wrongly ran v20.20.2 here).
out="$(plug "$(mkfix ge2099 '{"engines":{"node":">=20.99.0"}}')" -e PLUGIN_MANAGER=none -e PLUGIN_COMMAND='node --version')"
printf '%s\n' "$out" | grep -qE '^v22\.' || die "engines >=20.99.0 must skip node 20 (20.20.2 fails) and run node 22" "$out"

# 2b-3. a patch bound NO baked version can meet (">=24.99.0") must FAIL —
#       never fall back to a version that doesn't satisfy.
if plug "$(mkfix ge2499 '{"engines":{"node":">=24.99.0"}}')" -e PLUGIN_MANAGER=none -e PLUGIN_COMMAND='node --version' >/dev/null 2>&1; then
  die "engines >=24.99.0 must fail (no baked patch satisfies), not run a lower node"
fi

# 2c. an UPPER bound must be honored: "<22" excludes 22 → node 20 runs,
#     never the excluded major (the dangerous corner).
out="$(plug "$(mkfix lt22 '{"engines":{"node":"<22"}}')" -e PLUGIN_MANAGER=none -e PLUGIN_COMMAND='node --version')"
printf '%s\n' "$out" | grep -qE '^v20\.' || die "engines <22 must run node 20 (22 excluded), not the excluded major" "$out"

# 2d. a range NO baked major satisfies ("<20") must FAIL — never run an
#     excluded major just because its number happens to be baked.
if plug "$(mkfix lt20 '{"engines":{"node":"<20"}}')" -e PLUGIN_MANAGER=none -e PLUGIN_COMMAND='node --version' >/dev/null 2>&1; then
  die "engines <20 must fail loud, not run a baked (excluded) major"
fi

# 3. bun is baked and runnable.
out="$(plug "$(mkfix bun '{}')" -e PLUGIN_MANAGER=none -e PLUGIN_COMMAND='bun --version')"
printf '%s\n' "$out" | grep -qE '(^|[^0-9])1\.[0-9]+' || die "bun not runnable" "$out"

# 4. corepack pnpm with NO packageManager must NOT crash (the High bug:
#    `corepack prepare --activate` errored without a pin).
d="$(mkfix pnpm-nopin '{}')"; : >"$d/pnpm-lock.yaml"
out="$(plug "$d" -e PLUGIN_INSTALL=false -e PLUGIN_COMMAND='pnpm --version')"
printf '%s\n' "$out" | grep -qE '(^|[^0-9])[0-9]+\.[0-9]+\.[0-9]+' || die "pnpm without packageManager crashed" "$out"

# 5. corepack pnpm WITH packageManager pins the exact version.
d="$(mkfix pnpm-pin '{"packageManager":"pnpm@9.0.0"}')"; : >"$d/pnpm-lock.yaml"
out="$(plug "$d" -e PLUGIN_INSTALL=false -e PLUGIN_COMMAND='pnpm --version')"
printf '%s\n' "$out" | grep -q '9\.0\.0' || die "pnpm not pinned to 9.0.0" "$out"

# 6. the `nodejs` alias works (recreated as a RELATIVE symlink, not the
#    dangling absolute link the official image ships).
out="$(plug "$(mkfix nodejs '{}')" -e PLUGIN_MANAGER=none -e PLUGIN_COMMAND='nodejs --version')"
printf '%s\n' "$out" | grep -qE '^v22\.' || die "nodejs alias broken" "$out"

echo "PASS: node smoke"
