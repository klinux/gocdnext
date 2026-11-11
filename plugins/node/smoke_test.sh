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

echo "==> build (--no-cache, same Dockerfile/context as the official build)"
docker build --no-cache -t "${IMG}" "${HERE}" >/dev/null

# plug <fixture-dir> <env-flags...> — run the plugin against a fixture.
plug() {
  local dir="$1"; shift
  docker run --rm -v "${dir}:/workspace" -w /workspace "$@" "${IMG}" 2>&1
}
mkfix() { local d="${WORK}/$1"; mkdir -p "$d"; printf '%s' "$2" >"$d/package.json"; echo "$d"; }
die() { echo "FAIL: $1"; [ -n "${2:-}" ] && { echo "--- output ---"; echo "$2"; }; exit 1; }

# 0. base is trixie and the THREE OS packages the migration targets carry the
#    fix. This is the whole reason for the bookworm->trixie move: libsqlite3-0
#    (CVE-2025-7458), linux-libc-dev (CVE-2026-99083) and zlib1g
#    (CVE-2023-45853) have no bookworm fix. Compare with dpkg, NOT a regex:
#    Debian versions don't sort lexically (e.g. "3.5" < "3.46"), so a regex
#    floor would false-pass a lower version. Floors (first trixie fixed
#    version): zlib1g >= 1:1.3 (bookworm shipped 1:1.2.13), libsqlite3-0 >= 3.46
#    (bookworm shipped 3.40.x), linux-libc-dev >= 6.12.115-2 (CVE-2026-99083;
#    per the Debian security tracker, pulled in via build-essential -> libc6-dev).
osv="$(docker run --rm --network none --entrypoint sh "${IMG}" -c '
  printf "debian %s\n" "$(cat /etc/debian_version)"
  z="$(dpkg-query -W -f="\${Version}" zlib1g)"
  s="$(dpkg-query -W -f="\${Version}" libsqlite3-0)"
  l="$(dpkg-query -W -f="\${Version}" linux-libc-dev)"
  printf "zlib1g %s\n" "$z"; printf "libsqlite3-0 %s\n" "$s"; printf "linux-libc-dev %s\n" "$l"
  dpkg --compare-versions "$z" ge "1:1.3"     && echo ZLIB_FIXED       || echo ZLIB_UNFIXED
  dpkg --compare-versions "$s" ge "3.46"      && echo SQLITE_FIXED     || echo SQLITE_UNFIXED
  dpkg --compare-versions "$l" ge "6.12.115-2" && echo LINUX_LIBC_FIXED || echo LINUX_LIBC_UNFIXED
')" || die "OS probe failed" "$osv"
grep -qE '^debian 13\.'      <<<"$osv" || die "base is not Debian trixie (13.x)" "$osv"
grep -qx 'ZLIB_FIXED'        <<<"$osv" || die "zlib1g does not carry the CVE-2023-45853 fix (>= 1:1.3)" "$osv"
grep -qx 'SQLITE_FIXED'      <<<"$osv" || die "libsqlite3-0 does not carry the CVE-2025-7458 fix (>= 3.46)" "$osv"
grep -qx 'LINUX_LIBC_FIXED'  <<<"$osv" || die "linux-libc-dev does not carry the CVE-2026-99083 fix (>= 6.12.115-2)" "$osv"

# 1. node major selected from engines.node AND actually on PATH inside the
#    `bash -lc` the entrypoint execs (the /etc/profile-reset bug). Matrix is
#    22/24/26 — node 20 was dropped as EOL (#373; rejection proven below).
for maj in 22 24 26; do
  d="$(mkfix "n${maj}" "{\"engines\":{\"node\":\"${maj}.x\"}}")"
  out="$(plug "$d" -e PLUGIN_MANAGER=none -e PLUGIN_COMMAND='node --version')"
  echo "$out" | grep -qE "==> node: ${maj} " || die "engines ${maj}.x not selected" "$out"
  echo "$out" | grep -qE "^v${maj}\." || die "node v${maj} not on PATH in bash -lc" "$out"
done

# 1b. SECURITY INVARIANT (all majors): npm RUNS under the selected node, and
#     the node-tar it bundles is >= 7.5.19 — the removed CVE-2026-59873. This
#     drives the REAL path: the plugin selects the major and npm is EXECUTED
#     under THAT node (not the image default), so an npm that can't run under
#     the selected node fails HERE — as does a future base re-pin that
#     regresses tar. (version sort: semver is not zero-padded, so the 7.5.19
#     floor must sort first.)
#     Per-major npm: node 22 ships npm 10.x (tar 7.5.11, vulnerable) → WE bump
#     it to 11.19.1; node 24 already ships 11.19.1; node 26 ships 11.20.0
#     (tar 7.5.22) and needs no bump.
for maj in 22 24 26; do
  d="$(mkfix "ver${maj}" "{\"engines\":{\"node\":\"${maj}.x\"}}")"
  out="$(plug "$d" -e PLUGIN_MANAGER=none -e PLUGIN_COMMAND='npm --version')"
  printf '%s\n' "$out" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+$' \
    || die "node-${maj}: npm must run under the selected node" "$out"
  out="$(plug "$d" -e PLUGIN_MANAGER=none -e PLUGIN_COMMAND='cat "$(npm root -g)/npm/node_modules/tar/package.json"')"
  tv="$(printf '%s\n' "$out" | grep -oE '"version": *"[0-9]+\.[0-9]+\.[0-9]+"' | head -1 | grep -oE '[0-9]+\.[0-9]+\.[0-9]+')"
  [ -n "$tv" ] || die "node-${maj}: could not read the npm-bundled tar version" "$out"
  [ "$(printf '%s\n7.5.19\n' "$tv" | sort -V | head -1)" = "7.5.19" ] \
    || die "node-${maj}: npm-bundled tar ${tv} is < 7.5.19 (CVE-2026-59873)" "$out"
done

# 1c. node 22 specifically carries OUR explicit npm@11.19.1 bump (node 22's
#     shipped npm 10.x bundled a vulnerable tar) — assert the exact version so
#     a dropped/again-stale bump is caught, not just "some npm ran".
d="$(mkfix ver22exact '{"engines":{"node":"22.x"}}')"
out="$(plug "$d" -e PLUGIN_MANAGER=none -e PLUGIN_COMMAND='npm --version')"
printf '%s\n' "$out" | grep -qxF '11.19.1' \
  || die "node-22: our npm@11.19.1 bump must run under node 22" "$out"

# 1d. node 26 has corepack installed standalone (node stopped bundling it as of
#     node 25). The plugin drives pnpm/yarn through corepack, so it MUST be
#     present and runnable under the selected node 26.
d="$(mkfix cp26 '{"engines":{"node":"26.x"}}')"
out="$(plug "$d" -e PLUGIN_MANAGER=none -e PLUGIN_COMMAND='corepack --version')"
printf '%s\n' "$out" | grep -qE '(^|[^0-9])[0-9]+\.[0-9]+\.[0-9]+' \
  || die "node-26: corepack must be installed standalone and runnable" "$out"

# NOTE: always CAPTURE the plug output then grep the string — never
# `plug | grep -q`. grep -q closes the pipe on first match, the still-
# writing `docker run` takes SIGPIPE and exits 141, and `set -o pipefail`
# turns that into a (racy) false failure even though the match succeeded.

# 2. no declaration → default node 22.
out="$(plug "$(mkfix def '{}')" -e PLUGIN_MANAGER=none -e PLUGIN_COMMAND='node --version')"
printf '%s\n' "$out" | grep -qE '==> node: 22 \(default\)' || die "default node not 22" "$out"

# 2b. real semver eval (proves the bundled semver is found + used): an open
#     range widens to the SMALLEST baked major (now 22, was 20 pre-#373).
#     ">=18", operator SPACE ">= 18" and a HYPHEN range "18 - 24" are all
#     valid semver and must resolve to node 22 (the bash parser used to
#     mis-handle the spaced/hyphen forms).
for rng in '>=18' '>= 18' '18 - 24'; do
  out="$(plug "$(mkfix "ge_$$" "{\"engines\":{\"node\":\"${rng}\"}}")" -e PLUGIN_MANAGER=none -e PLUGIN_COMMAND='node --version')"
  printf '%s\n' "$out" | grep -qE '==> node: 22 ' || die "engines '${rng}' should widen to node 22" "$out"
  printf '%s\n' "$out" | grep -qE '^v22\.' || die "engines '${rng}' should actually run node 22" "$out"
done

# 2b-2. MINOR/PATCH honored: ">=22.99.0" is NOT met by baked 22 (22.23.x)
#       but IS met by 24 → must skip 22 and pick node 24 (a major-only
#       parser would wrongly run 22 here).
out="$(plug "$(mkfix ge2299 '{"engines":{"node":">=22.99.0"}}')" -e PLUGIN_MANAGER=none -e PLUGIN_COMMAND='node --version')"
printf '%s\n' "$out" | grep -qE '^v24\.' || die "engines >=22.99.0 must skip node 22 (22.23.x fails) and run node 24" "$out"

# 2b-3. a patch bound NO baked version can meet (">=26.99.0") must FAIL —
#       never fall back to a version that doesn't satisfy.
if plug "$(mkfix ge2699 '{"engines":{"node":">=26.99.0"}}')" -e PLUGIN_MANAGER=none -e PLUGIN_COMMAND='node --version' >/dev/null 2>&1; then
  die "engines >=26.99.0 must fail (no baked patch satisfies), not run a lower node"
fi

# 2c. an UPPER bound must be honored: "<24" excludes 24 and 26 → node 22 runs,
#     never an excluded major (the dangerous corner).
out="$(plug "$(mkfix lt24 '{"engines":{"node":"<24"}}')" -e PLUGIN_MANAGER=none -e PLUGIN_COMMAND='node --version')"
printf '%s\n' "$out" | grep -qE '^v22\.' || die "engines <24 must run node 22 (24/26 excluded), not an excluded major" "$out"

# 2e. #373 — node 20 is DROPPED. Every channel that resolves to 20 (or to a
#     range no baked major satisfies) must FAIL LOUD (exit 2) and NEVER run
#     the command. assert_rejected proves all three: exit 2, no `vXX.X` line
#     (the `node --version` command never executed), and a migration hint.
assert_rejected() { # <label> <fixture-dir> <extra -e ...>
  local label="$1" dir="$2"; shift 2
  local out rc
  set +e
  out="$(docker run --rm -v "${dir}:/workspace" -w /workspace "$@" \
        -e PLUGIN_MANAGER=none -e PLUGIN_COMMAND='node --version' "${IMG}" 2>&1)"
  rc=$?
  set -e
  [ "$rc" -eq 2 ] || die "${label}: expected exit 2, got ${rc}" "$out"
  printf '%s\n' "$out" | grep -qE '^v[0-9]' && die "${label}: node ran despite the rejection (command executed)" "$out"
  printf '%s\n' "$out" | grep -qiE 'baked|custom image|satisf|EOL' || die "${label}: missing migration guidance" "$out"
}
# explicit node-version:20
assert_rejected "node-version:20" "$(mkfix rej_nv20 '{}')" -e PLUGIN_NODE_VERSION=20
# engines.node "20.x" (a range no baked major satisfies)
assert_rejected "engines:20.x" "$(mkfix rej_eng20 '{"engines":{"node":"20.x"}}')"
# .nvmrc 20 (exact unbaked major)
dn="$(mkfix rej_nvmrc20 '{}')"; printf '20\n' >"${dn}/.nvmrc"
assert_rejected ".nvmrc:20" "$dn"
# a "<22" range — no baked major satisfies it anymore (was node 20 pre-#373)
assert_rejected "engines:<22" "$(mkfix rej_lt22 '{"engines":{"node":"<22"}}')"

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
