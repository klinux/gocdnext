#!/usr/bin/env bash
# Mock-PATH test for the node plugin entrypoint. Stubs node / pnpm /
# npm / yarn / bun / corepack / git on PATH (each appends its argv to a
# file) and injects a temp NODE_BASE_DIR with fake node-20/22/24 so
# select-node.sh's version switch runs without a real image. Asserts:
#   - version selection from engines.node / .nvmrc / node-version input,
#     including the unbaked/malformed fail-loud paths;
#   - bun detection + install dialect (and that corepack is NOT used);
#   - the pnpm/npm regressions still behave.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# ── stub tools on PATH: record argv as "<tool> <args>" ──
STUB="$TMP/bin"
mkdir -p "$STUB"
for tool in pnpm npm yarn bun corepack; do
    cat >"$STUB/$tool" <<EOF
#!/usr/bin/env bash
printf '${tool} %s\n' "\$*" >> "\$ARGS_FILE"
EOF
    chmod +x "$STUB/$tool"
done
# git: no-op (entrypoint runs \`git config --global ...\`).
printf '#!/usr/bin/env bash\nexit 0\n' >"$STUB/git"
chmod +x "$STUB/git"

# ── fake baked node majors (only need an executable node-*/bin/node) ──
NODEBASE="$TMP/opt-node"
for maj in 20 22 24; do
    mkdir -p "$NODEBASE/node-$maj/bin"
    printf '#!/usr/bin/env bash\nexit 0\n' >"$NODEBASE/node-$maj/bin/node"
    chmod +x "$NODEBASE/node-$maj/bin/node"
done

# run <workdir-setup-fn> [extra env assignments...] — sets up a fresh
# work dir, runs the entrypoint under the stub PATH, captures stdout+rc.
OUT=""
RC=0
run() {
    local work="$TMP/work"
    rm -rf "$work"; mkdir -p "$work"
    "$1" "$work"                      # test-provided fixture writer
    shift
    : >"$TMP/args"
    OUT="$(cd "$work" && env ARGS_FILE="$TMP/args" NODE_BASE_DIR="$NODEBASE" \
        PATH="$STUB:$NODEBASE/node-22/bin:/usr/bin:/bin" \
        "$@" bash "$HERE/entrypoint.sh" 2>&1)"
    RC=$?
}
fail() { echo "FAIL: $1"; echo "--- output ---"; echo "$OUT"; echo "--- args ---"; cat "$TMP/args" 2>/dev/null; exit 1; }

pkg() { printf '%s' "$2" >"$1/package.json"; }

# ─────────────────────────────── version ───────────────────────────────

# 1. engines.node "24.x" → node 24, bun install (needs a manager so the
#    install-only job is valid).
f_engines24() { pkg "$1" '{"engines":{"node":"24.x"},"packageManager":"bun@1.3.9"}'; }
run f_engines24
[ "$RC" -eq 0 ] || fail "engines.node 24.x should succeed (rc=$RC)"
echo "$OUT" | grep -qE '==> node: 24 \(engines\.node' || fail "engines.node 24.x should select node 24"

# 2. .nvmrc "20" → node 20.
f_nvmrc20() { printf '20\n' >"$1/.nvmrc"; printf '{"packageManager":"bun@1.3.9"}\n' >"$1/package.json"; }
run f_nvmrc20
echo "$OUT" | grep -qE '==> node: 20 \(\.node-version/\.nvmrc' || fail ".nvmrc 20 should select node 20"

# 3. explicit node-version:22 OVERRIDES engines.node 24 (both printed).
run f_engines24 PLUGIN_NODE_VERSION=22
echo "$OUT" | grep -qE '==> node: 22 \(node-version input' || fail "node-version:22 should override to node 22"
echo "$OUT" | grep -qi 'overrides engines.node' || fail "override divergence should be printed"

# 4. engines.node "18" → unbaked → exit 2 (never silent fall-back).
f_engines18() { pkg "$1" '{"engines":{"node":"18.x"}}'; }
run f_engines18
[ "$RC" -eq 2 ] || fail "unbaked node 18 must exit 2 (got $RC)"
echo "$OUT" | grep -qiE "satisf|not baked" || fail "unbaked should fail loud with a clear reason ($OUT)"

# 5. no declaration → default node 22.
f_bare() { pkg "$1" '{"packageManager":"bun@1.3.9"}'; }
run f_bare
echo "$OUT" | grep -qE '==> node: 22 \(default\)' || fail "no declaration should default to node 22"

# 6. engines.node with no numeric major (an nvm alias like "lts/*") is a
#    hint we can't resolve → fall back to the default node (22), NOT a
#    hard fail. A parseable-but-unbaked major (test 4) is the one that
#    fails loud.
f_alias() { pkg "$1" '{"engines":{"node":"lts/*"},"packageManager":"bun@1.3.9"}'; }
run f_alias
[ "$RC" -eq 0 ] || fail "alias engines.node should fall back to default, not fail (got $RC)"
echo "$OUT" | grep -qE '==> node: 22 \(default\)' || fail "unparseable engines.node should use the default node"

# 6b. .nvmrc with no numeric major → default too (CONSISTENT with engines;
#     a declarative hint is never a hard fail).
f_nvmrc_alias() { printf 'lts/*\n' >"$1/.nvmrc"; pkg "$1" '{"packageManager":"bun@1.3.9"}'; }
run f_nvmrc_alias
[ "$RC" -eq 0 ] || fail ".nvmrc alias should fall back to default, not fail (got $RC)"
echo "$OUT" | grep -qE '==> node: 22 \(default\)' || fail ".nvmrc alias should use the default node"

# 6c. EXPLICIT node-version garbage IS a hard fail — the operator's
#     intent, unlike a declarative hint.
run f_bare PLUGIN_NODE_VERSION=lts
[ "$RC" -eq 2 ] || fail "explicit node-version garbage must exit 2 (got $RC)"

# 6d. engines.node RANGES are evaluated with the image's REAL semver
#     (minor/patch, operator spaces, hyphen ranges all honored). The mock
#     has no node/semver, so a range degrades to a LOUD failure here
#     (NOSEMVER) — never a mis-resolve. The actual range matrix
#     (">=18"→20, ">= 18"→20, "18 - 22"→20, "<22"→20, ">=20.99.0"→22,
#     "<20"→fail, ...) is covered against the real image in smoke_test.sh.
f_range() { pkg "$1" '{"engines":{"node":">=18"}}'; }
run f_range
[ "$RC" -eq 2 ] || fail "engines range without semver must fail loud, not mis-resolve (got $RC)"

# 6f. .nvmrc with an exact unbaked major fails loud (no range widening for
#     a pinned file version).
f_nvmrc18() { printf '18\n' >"$1/.nvmrc"; pkg "$1" '{"packageManager":"bun@1.3.9"}'; }
run f_nvmrc18
[ "$RC" -eq 2 ] || fail ".nvmrc 18 (exact unbaked) must exit 2 (got $RC)"

# ─────────────────────────────── bun ───────────────────────────────────

# 7. bun.lock → bun install --frozen-lockfile, corepack NOT invoked.
f_bunlock() { printf '' >"$1/bun.lock"; pkg "$1" '{}'; }
run f_bunlock
[ "$RC" -eq 0 ] || fail "bun.lock run should succeed (rc=$RC)"
grep -qE '^bun install --frozen-lockfile' "$TMP/args" || fail "bun.lock should run bun install --frozen-lockfile"
grep -q '^corepack' "$TMP/args" && fail "corepack must NOT be invoked for bun"

# 8. packageManager bun@x with no lockfile → detected as bun.
run f_bare
grep -qE '^bun install' "$TMP/args" || fail "packageManager bun@ should be detected as bun"

# 9. frozen:false drops --frozen-lockfile.
run f_bunlock PLUGIN_FROZEN=false
grep -qE '^bun install --frozen-lockfile' "$TMP/args" && fail "frozen:false must drop --frozen-lockfile"
grep -qE '^bun install$' "$TMP/args" || fail "frozen:false should be plain bun install"

# 10. prod:true → --production.
run f_bunlock PLUGIN_PROD=true
grep -qE '^bun install .*--production' "$TMP/args" || fail "prod:true should pass --production to bun"

# ─────────────────────────── regressions ───────────────────────────────

# 11. pnpm-lock.yaml, NO packageManager → corepack enable but NOT
#     `corepack prepare --activate` (errors without a pin) + a warn;
#     frozen install still runs.
f_pnpm() { printf '' >"$1/pnpm-lock.yaml"; pkg "$1" '{"engines":{"node":"22.x"}}'; }
run f_pnpm
grep -qE '^corepack enable' "$TMP/args" || fail "pnpm should corepack enable"
grep -qE '^corepack prepare' "$TMP/args" && fail "no packageManager → must NOT corepack prepare --activate"
echo "$OUT" | grep -qi 'no packageManager' || fail "missing packageManager should warn"
grep -qE '^pnpm install --frozen-lockfile' "$TMP/args" || fail "pnpm should run frozen install"

# 11b. pnpm-lock.yaml WITH packageManager → corepack prepare --activate
#      (deterministic, pinned version).
f_pnpm_pin() { printf '' >"$1/pnpm-lock.yaml"; pkg "$1" '{"packageManager":"pnpm@9.0.0"}'; }
run f_pnpm_pin
grep -qE '^corepack prepare --activate' "$TMP/args" || fail "packageManager pin → corepack prepare --activate"

# 11c. lockfile/packageManager conflict → exit 2 (pnpm-lock but the
#      packageManager field names a different tool). corepack would
#      otherwise choke or prepare the wrong tool.
f_conflict() { printf '' >"$1/pnpm-lock.yaml"; pkg "$1" '{"packageManager":"bun@1.4.0"}'; }
run f_conflict
[ "$RC" -eq 2 ] || fail "lockfile/packageManager conflict must exit 2 (got $RC)"
echo "$OUT" | grep -qi 'conflict' || fail "conflict should be explained"

# 12. package-lock.json → npm ci.
f_npm() { printf '' >"$1/package-lock.json"; pkg "$1" '{}'; }
run f_npm
grep -qE '^npm ci' "$TMP/args" || fail "npm should run npm ci (frozen)"

echo "PASS: node entrypoint"
