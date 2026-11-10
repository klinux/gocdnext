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

# --- #335: cache: bucket keeps the AWS secret + session token off every argv --
# The bucket path runs a self-managed BuildKit (remote driver) and hands the
# credentials to it through a docker cp'd shared-credentials FILE — never via
# `--driver-opt env.AWS_*` (which lands the value on `docker buildx create`'s
# argv) nor `docker -e NAME=VALUE` (which stores it in Config.Env). The mock
# docker records every call's argv in all.log; the sentinels must appear in
# NONE of them. (Real Config.Env + end-to-end S3 cache are the smoke's job.)
SECRET_SENTINEL="SENTINELsecretAKIAzzz999"
TOKEN_SENTINEL="SENTINELsessiontokenTTT888"

run_bucket() { # run_bucket <logfile> <extra env NAME=VALUE ...>
    local lf="$1"; shift
    : >"$TMP/all.log"; : >"$TMP/build.log"
    env PLUGIN_IMAGE="img.example.com/org/app" PLUGIN_TAGS="1.0.0" \
        PLUGIN_CONTEXT="$TMP" PLUGIN_CACHE="bucket" \
        GOCDNEXT_LAYER_CACHE_BUCKET="test-bucket" \
        AWS_ACCESS_KEY_ID="AKIAEXAMPLEPUBLICID" \
        AWS_SECRET_ACCESS_KEY="${SECRET_SENTINEL}" \
        AWS_SESSION_TOKEN="${TOKEN_SENTINEL}" \
        "$@" PATH="$TMP:$PATH" bash "$HERE/entrypoint.sh" >/dev/null 2>&1
    local rc=$?
    cp "$TMP/all.log" "$lf" 2>/dev/null || true
    return $rc
}

# s3 (AWS-native) backend: remote driver, no secret anywhere on argv.
run_bucket "$TMP/bucket.log" || fail "bucket path: entrypoint exited non-zero"
grep -q -- "--driver remote" "$TMP/bucket.log" || fail "bucket: remote driver not used"
grep -q "docker-container://" "$TMP/bucket.log" || fail "bucket: docker-container:// endpoint not used"
grep -q "buildx create .*--driver-opt env.AWS" "$TMP/bucket.log" \
    && fail "bucket: AWS env still passed via --driver-opt (argv leak)"
grep -qF "${SECRET_SENTINEL}" "$TMP/bucket.log" \
    && fail "bucket: AWS_SECRET_ACCESS_KEY value leaked onto a docker argv"
grep -qF "${TOKEN_SENTINEL}" "$TMP/bucket.log" \
    && fail "bucket: AWS_SESSION_TOKEN value leaked onto a docker argv"
grep -q "create --rm --privileged --name gocdnext-buildkitd" "$TMP/bucket.log" \
    || fail "bucket: self-managed buildkitd container not created"
grep -qE "cp .*/gocdnext gocdnext-buildkitd-[0-9]+:/etc/" "$TMP/bucket.log" \
    || fail "bucket: credentials/config not staged via docker cp"
grep -q "AWS_SHARED_CREDENTIALS_FILE=/etc/gocdnext/aws-credentials" "$TMP/bucket.log" \
    || fail "bucket: AWS_SHARED_CREDENTIALS_FILE path not set on the builder"
# the self-managed builder uses the pinned buildkit image, not a floating tag.
grep -qF "${BUILDKIT_PIN}" "$TMP/bucket.log" || fail "bucket: self-managed buildkit not pinned"

echo "OK: buildx cache:bucket keeps AWS secret + session token off argv (#335)"

# Regression: NO session token (the common static-key / GCS-HMAC path). The
# credentials subshell must not end on `[ -n token ] && printf`, whose false
# result would be the group's exit status and trip `set -e`. Run WITHOUT
# AWS_SESSION_TOKEN and require a clean exit + creds still staged.
: >"$TMP/all.log"; : >"$TMP/build.log"
env PLUGIN_IMAGE="img.example.com/org/app" PLUGIN_TAGS="1.0.0" \
    PLUGIN_CONTEXT="$TMP" PLUGIN_CACHE="bucket" \
    GOCDNEXT_LAYER_CACHE_BUCKET="test-bucket" \
    AWS_ACCESS_KEY_ID="AKIAEXAMPLEPUBLICID" \
    AWS_SECRET_ACCESS_KEY="${SECRET_SENTINEL}" \
    PATH="$TMP:$PATH" bash "$HERE/entrypoint.sh" >/dev/null 2>&1 \
    || fail "bucket/no-token: entrypoint exited non-zero (creds subshell poisoned \$? under set -e)"
grep -q "AWS_SHARED_CREDENTIALS_FILE=/etc/gocdnext/aws-credentials" "$TMP/all.log" \
    || fail "bucket/no-token: credentials file not staged"
echo "OK: buildx cache:bucket stages creds with NO session token (set -e regression, #335)"

# Partial credentials must FAIL FAST with EXIT 2, before any docker call (#335):
# a half-set pair, or a session token without the pair, would otherwise drop both
# and fall back to the SDK default chain (a different identity) instead of
# erroring. The mock docker logs every call to all.log, so an empty all.log
# proves the guard ran ahead of the daemon wait.
part_run() { # single env assignment as $1
  : >"$TMP/all.log"
  env PLUGIN_IMAGE="img.example.com/org/app" PLUGIN_TAGS="1.0.0" PLUGIN_CONTEXT="$TMP" \
      PLUGIN_CACHE="bucket" GOCDNEXT_LAYER_CACHE_BUCKET="test-bucket" \
      "$1" PATH="$TMP:$PATH" bash "$HERE/entrypoint.sh" >/dev/null 2>&1
}
for ce in "AWS_ACCESS_KEY_ID=AKIAEXAMPLEPUBLICID" \
          "AWS_SECRET_ACCESS_KEY=${SECRET_SENTINEL}" \
          "AWS_SESSION_TOKEN=${TOKEN_SENTINEL}"; do
  part_run "$ce"; rc=$?
  [ "$rc" = 2 ] || fail "partial cred [$ce]: expected exit 2, got $rc"
  [ -s "$TMP/all.log" ] && fail "partial cred [$ce]: a docker command ran before the guard"
done
echo "OK: buildx cache:bucket rejects partial credentials with exit 2, pre-docker (#335)"

# Positive control: the FULLY-ABSENT trio on the s3 backend must PROCEED to the
# builder — the default chain stays allowed (the guard rejects partial, not
# empty). Prove the self-managed builder was created and no creds-file path was set.
: >"$TMP/all.log"
env PLUGIN_IMAGE="img.example.com/org/app" PLUGIN_TAGS="1.0.0" PLUGIN_CONTEXT="$TMP" \
    PLUGIN_CACHE="bucket" GOCDNEXT_LAYER_CACHE_BUCKET="test-bucket" \
    PATH="$TMP:$PATH" bash "$HERE/entrypoint.sh" >/dev/null 2>&1 \
    || fail "no-credentials (default chain) should proceed, not fail"
grep -q "create --rm --privileged --name gocdnext-buildkitd" "$TMP/all.log" \
    || fail "no-credentials: self-managed builder was not created (default chain not allowed)"
grep -q "AWS_SHARED_CREDENTIALS_FILE" "$TMP/all.log" \
    && fail "no-credentials: a credentials-file path was set despite no creds"
echo "OK: buildx cache:bucket allows the default chain when no creds are set (#335)"

# gcs (S3-interop) backend: checksum opt-out is preserved — as NON-secret env on
# the builder container — while the secret still never hits an argv.
run_bucket "$TMP/bucket-gcs.log" GOCDNEXT_LAYER_CACHE_BACKEND="gcs" \
    || fail "bucket/gcs path: entrypoint exited non-zero"
grep -q "AWS_REQUEST_CHECKSUM_CALCULATION=when_required" "$TMP/bucket-gcs.log" \
    || fail "bucket/gcs: checksum opt-out not set on the builder"
grep -q "AWS_RESPONSE_CHECKSUM_VALIDATION=when_required" "$TMP/bucket-gcs.log" \
    || fail "bucket/gcs: checksum response-validation opt-out not set"
grep -qF "${SECRET_SENTINEL}" "$TMP/bucket-gcs.log" \
    && fail "bucket/gcs: AWS_SECRET_ACCESS_KEY value leaked onto a docker argv"

echo "OK: buildx cache:bucket (gcs) preserves checksum opt-out without leaking the secret (#335)"

# --- #335: opt-in S3 path-style addressing (GOCDNEXT_LAYER_CACHE_USE_PATH_STYLE)
# MinIO and other S3-compatible stores without bucket.<endpoint> virtual-host DNS
# need path-style; AWS/GCS/R2 don't. The knob is EXPLICIT opt-in (never inferred
# from the endpoint), must land on BOTH cache-to and cache-from, and a bad value
# must fail before the self-managed builder is created. The buildx build argv is
# captured in build.log.
# (a) custom endpoint, no opt-in → the attribute is NOT emitted.
run_bucket "$TMP/bucket-nops.log" GOCDNEXT_LAYER_CACHE_ENDPOINT="http://minio:9000" \
    || fail "path-style/off: entrypoint exited non-zero"
grep -q "use_path_style" "$TMP/build.log" \
    && fail "path-style/off: use_path_style emitted without opt-in"
# (b) explicit true → use_path_style=true on cache-to AND cache-from, exactly
#     twice on the argv (a half-applied style silently breaks the import side).
run_bucket "$TMP/bucket-ps.log" GOCDNEXT_LAYER_CACHE_ENDPOINT="http://minio:9000" \
    GOCDNEXT_LAYER_CACHE_USE_PATH_STYLE="true" \
    || fail "path-style/on: entrypoint exited non-zero"
grep -q -- "--cache-to .*use_path_style=true" "$TMP/build.log" \
    || fail "path-style/on: use_path_style missing from cache-to"
grep -q -- "--cache-from .*use_path_style=true" "$TMP/build.log" \
    || fail "path-style/on: use_path_style missing from cache-from"
ps_count="$(grep -o "use_path_style=true" "$TMP/build.log" | wc -l | tr -d ' ')"
[ "$ps_count" = "2" ] \
    || fail "path-style/on: expected use_path_style=true exactly twice (to+from), got ${ps_count}"
# (c) invalid value → exit 2, BEFORE any self-managed builder is created.
if run_bucket "$TMP/bucket-bad.log" GOCDNEXT_LAYER_CACHE_USE_PATH_STYLE="yes"; then
    fail "path-style/bad: expected non-zero exit for an invalid value"
fi
grep -q "create --rm --privileged --name gocdnext-buildkitd" "$TMP/bucket-bad.log" \
    && fail "path-style/bad: builder was created despite the invalid value"

echo "OK: buildx cache:bucket path-style is opt-in, dual-spec, and validated (#335)"
