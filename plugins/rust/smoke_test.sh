#!/usr/bin/env bash
# Real smoke for the rust plugin — builds the PINNED IMAGE and runs the actual
# cargo/clippy/rustfmt toolchain against throwaway crates (stdlib only, no
# network), so the 1.83→1.99 toolchain jump is proven functional before publish:
#   - cargo --version reports the new toolchain;
#   - a clean crate builds, lints clean under `clippy -- -D warnings`, and passes
#     `fmt --check`;
#   - clippy actually catches a lint (fails `-D warnings`), i.e. it's not a no-op;
#   - PLUGIN_COMMAND is required; PLUGIN_WORKING_DIR is honored.
# Convention-named so plugins.yml runs it. Needs docker.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMG="gocdnext-plugin-rust:smoke-$$"
WORK="$(mktemp -d)"
cleanup() {
  # plug runs as the host user, so target/ is host-owned and a plain rm removes
  # it — no `|| true` mask: a cleanup failure is a real leak worth surfacing.
  rm -rf "$WORK"
  docker image rm -f "$IMG" >/dev/null 2>&1 || true
}
trap cleanup EXIT

die() { echo "FAIL: $1"; [ -n "${2:-}" ] && { echo "--- output ---"; echo "$2"; }; exit 1; }

echo "==> build (same Dockerfile/context as the official build)"
docker build -t "$IMG" "$HERE" >/dev/null

# a clean, clippy- and rustfmt-correct crate
CLEAN="$WORK/clean"; mkdir -p "$CLEAN/src"
cat >"$CLEAN/Cargo.toml" <<'EOF'
[package]
name = "smoke"
version = "0.1.0"
edition = "2021"

[lib]
path = "src/lib.rs"
EOF
cat >"$CLEAN/src/lib.rs" <<'EOF'
/// Adds two numbers.
pub fn add(a: i32, b: i32) -> i32 {
    a + b
}
EOF

# a crate with a default clippy lint (needless_return) — fails `-D warnings`
DIRTY="$WORK/dirty"; mkdir -p "$DIRTY/src"
cp "$CLEAN/Cargo.toml" "$DIRTY/Cargo.toml"
cat >"$DIRTY/src/lib.rs" <<'EOF'
pub fn one() -> i32 {
    return 1;
}
EOF

# plug <crate-dir> <command> -> OUT, RC (never trips set -e)
# Run as the host user so cargo's target/ (written into the mount) stays
# host-owned — otherwise root-owned build artifacts break cleanup and later host
# reads. cargo reads the baked toolchain (world-readable) and writes only
# CARGO_HOME + target under the workspace, so non-root is fine for stable builds.
plug() {
  local dir="$1" cmd="$2"
  set +e
  OUT="$(docker run --rm --user "$(id -u):$(id -g)" -e HOME=/tmp \
    -v "$dir:/workspace" -w /workspace -e PLUGIN_COMMAND="$cmd" "$IMG" 2>&1)"; RC=$?
  set -e
}

# 1. toolchain present + version is the new 1.99.x.
plug "$CLEAN" "--version"
[ "$RC" -eq 0 ] || die "cargo --version should run (rc=$RC)" "$OUT"
grep -qE "cargo 1\.99\." <<<"$OUT" || die "unexpected cargo version (want 1.99.x)" "$OUT"

# 2. PLUGIN_COMMAND required.
plug "$CLEAN" ""
[ "$RC" -eq 2 ] || die "missing PLUGIN_COMMAND should exit 2 (rc=$RC)" "$OUT"

# 3. clean crate builds.
plug "$CLEAN" "build"
[ "$RC" -eq 0 ] || die "clean crate should build (rc=$RC)" "$OUT"

# 4. clean crate passes clippy -D warnings.
plug "$CLEAN" "clippy --all-targets -- -D warnings"
[ "$RC" -eq 0 ] || die "clean crate should pass clippy -D warnings (rc=$RC)" "$OUT"

# 5. clean crate passes fmt --check.
plug "$CLEAN" "fmt --check"
[ "$RC" -eq 0 ] || die "clean crate should pass fmt --check (rc=$RC)" "$OUT"

# 6. clippy actually lints: the dirty crate fails -D warnings (not a no-op).
plug "$DIRTY" "clippy --all-targets -- -D warnings"
[ "$RC" -ne 0 ] || die "clippy should fail -D warnings on needless_return crate" "$OUT"
grep -qi "needless_return\|needless return" <<<"$OUT" || die "expected the needless_return lint to be reported" "$OUT"

# 7. PLUGIN_WORKING_DIR: crate in a nested dir builds via working-dir. Build the
#    crate from source (don't copy CLEAN's target/ — just the sources).
NEST="$WORK/nest"; mkdir -p "$NEST/app/src"
cp "$CLEAN/Cargo.toml" "$NEST/app/Cargo.toml"
cp "$CLEAN/src/lib.rs" "$NEST/app/src/lib.rs"
set +e
OUT="$(docker run --rm --user "$(id -u):$(id -g)" -e HOME=/tmp \
    -v "$NEST:/workspace" -w /workspace \
    -e PLUGIN_WORKING_DIR=/workspace/app -e PLUGIN_COMMAND=build "$IMG" 2>&1)"; RC=$?
set -e
[ "$RC" -eq 0 ] || die "working-dir build should succeed (rc=$RC)" "$OUT"

echo "PASS: rust smoke"
