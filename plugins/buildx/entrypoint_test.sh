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

# Mock docker: every subcommand succeeds. The `buildx build` argv goes to
# build.log (push/compression wiring); the FULL argv of EVERY call goes to
# all.log (so we can also assert the pinned binfmt install + the buildkit image
# passed to `buildx create`). Covers info (daemon wait), buildx create/inspect/
# rm (builder lifecycle), the binfmt `run --privileged`, and build.
cat >"$TMP/docker" <<EOF
#!/usr/bin/env bash
echo "\$*" >> "$TMP/all.log"
if [ "\${1:-}" = "buildx" ] && [ "\${2:-}" = "build" ]; then
  echo "\$*" >> "$TMP/build.log"
fi
exit 0
EOF
chmod +x "$TMP/docker"

BINFMT_DIGEST="sha256:400a4873b838d1b89194d982c45e5fb3cda4593fbfd7e08a02e76b03b21166f0"
BUILDKIT_PIN="moby/buildkit:v0.34.2@sha256:c5f8862f52499773af85bc07695b2379ef08e8002352b0df2c4864c91d78dabe"

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

# --- pinned supply-chain images ---------------------------------------------
# A cross-arch target forces the binfmt install; buildx create always carries
# the builder image. Assert both are pinned by digest, not a floating tag.
run_full() { # run_full <logfile> <extra env NAME=VALUE ...>
    local lf="$1"; shift
    : >"$TMP/all.log"; : >"$TMP/build.log"
    env PLUGIN_IMAGE="img.example.com/org/app" PLUGIN_TAGS="1.0.0" \
        PLUGIN_CONTEXT="$TMP" PLUGIN_PLATFORMS="linux/amd64,linux/arm64" \
        "$@" PATH="$TMP:$PATH" bash "$HERE/entrypoint.sh" >/dev/null 2>&1
    cp "$TMP/all.log" "$lf" 2>/dev/null || true
}

# default: binfmt pinned by digest (privileged install), buildkit pinned.
run_full "$TMP/pin.log"
grep -q -- "--privileged" "$TMP/pin.log" || fail "cross-arch: binfmt install (privileged) did not run"
grep -q "tonistiigi/binfmt@${BINFMT_DIGEST} --install all" "$TMP/pin.log" \
    || fail "binfmt not pinned by digest"
if grep -qE "tonistiigi/binfmt(:[^@ ]+)? --install" "$TMP/pin.log"; then
    fail "binfmt uses a floating tag instead of @digest"
fi
grep -q "buildx create" "$TMP/pin.log" || fail "buildx create was not called"
grep -q "image=${BUILDKIT_PIN}" "$TMP/pin.log" \
    || fail "default buildkit not pinned to ${BUILDKIT_PIN}"
if grep -q "image=moby/buildkit:buildx-stable-1" "$TMP/pin.log"; then
    fail "buildkit fell back to the floating buildx-stable-1"
fi

# operator override replaces the default pin, verbatim.
run_full "$TMP/ovr.log" PLUGIN_BUILDKIT_IMAGE="moby/buildkit:v0.99.0"
grep -q "image=moby/buildkit:v0.99.0" "$TMP/ovr.log" \
    || fail "PLUGIN_BUILDKIT_IMAGE override was not honoured"
grep -q "image=${BUILDKIT_PIN}" "$TMP/ovr.log" \
    && fail "override must replace the default pin, not coexist with it"

echo "OK: buildx pinned binfmt + buildkit"
