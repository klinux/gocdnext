#!/usr/bin/env bash
# Mock-PATH test for the buildx plugin's `compression` codec wiring (#274).
# Locks the contract the CI image-build alone doesn't cover:
#   "" / gzip      -> `--push` (unchanged, back-compat)
#   zstd/uncompressed/estargz -> `--output type=image,compression=<c>,push=true`
#                                 and NOT a bare `--push`
#   unknown        -> exit 2 up front (fail-fast, before any docker call)
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# Mock docker: every subcommand succeeds; the `buildx build` argv is logged so
# we can assert how push/compression were wired. Covers info (daemon wait),
# buildx create/inspect/rm (builder lifecycle) and build.
cat >"$TMP/docker" <<EOF
#!/usr/bin/env bash
if [ "\${1:-}" = "buildx" ] && [ "\${2:-}" = "build" ]; then
  echo "\$*" >> "$TMP/build.log"
fi
exit 0
EOF
chmod +x "$TMP/docker"

run() { # $1 = PLUGIN_COMPRESSION value ("" allowed)
    : >"$TMP/build.log"
    PLUGIN_IMAGE="img.example.com/org/app" PLUGIN_TAGS="1.0.0" \
        PLUGIN_CONTEXT="$TMP" PLUGIN_COMPRESSION="$1" \
        PATH="$TMP:$PATH" bash "$HERE/entrypoint.sh" >/dev/null 2>&1
}

fail() { echo "FAIL: $1"; echo "--- build.log ---"; cat "$TMP/build.log" 2>/dev/null; exit 1; }

# default (empty) -> --push, no compression
run ""
grep -qw -- "--push" "$TMP/build.log" || fail "empty: expected --push"
grep -q "compression=" "$TMP/build.log" && fail "empty: must not set compression"

# gzip -> --push (back-compat)
run "gzip"
grep -qw -- "--push" "$TMP/build.log" || fail "gzip: expected --push"
grep -q "compression=" "$TMP/build.log" && fail "gzip: must not set compression"

# zstd -> explicit output, NOT a bare --push
run "zstd"
grep -q "type=image,compression=zstd,push=true" "$TMP/build.log" || fail "zstd: output spec missing"
grep -qw -- "--push" "$TMP/build.log" && fail "zstd: must not also pass --push"

# uncompressed / estargz -> explicit output with the codec
run "uncompressed"
grep -q "type=image,compression=uncompressed,push=true" "$TMP/build.log" || fail "uncompressed: output spec missing"
run "estargz"
grep -q "type=image,compression=estargz,push=true" "$TMP/build.log" || fail "estargz: output spec missing"

# unknown -> non-zero exit, and it fails BEFORE any docker build call
if run "lz4"; then fail "lz4: expected non-zero exit"; fi
[ -s "$TMP/build.log" ] && fail "lz4: must not reach the build (fail-fast)"

echo "OK: buildx compression wiring"
