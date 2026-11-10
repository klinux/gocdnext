#!/usr/bin/env bash
# Real smoke for the buildx plugin — a genuine multi-arch build + push + layer
# cache round-trip. Stands up a throwaway docker:24 DinD (the gocdnext k8s
# runtime default) and proves END TO END:
#   - the pinned binfmt image registers QEMU so an amd64 runner builds arm64,
#   - a linux/amd64,linux/arm64 build PUSHES a manifest list that really
#     contains BOTH architectures (read back off the registry),
#   - PLUGIN_CACHE=registry exports a cache the SECOND build reuses (CACHED).
#
# Topology note: buildx's builder is a SEPARATE BuildKit container the daemon
# spawns (not the daemon itself), so it does not honour the daemon's
# --insecure-registry. We therefore give the throwaway registry a TLS cert and
# hand the builder a BuildKit image with that CA baked in, via the plugin's own
# PLUGIN_BUILDKIT_IMAGE override — which FROMs the pinned default buildkit, so
# the real multi-arch/cache path runs on the pinned version. Everything
# registry-side lives INSIDE the dind daemon (the builder reaches it through the
# dind bridge gateway); the plugin runs as the client over DOCKER_HOST.
#
# Convention-named so plugins.yml runs it as a PRE-PUBLISH gate. Needs docker
# (privileged DinD — fine on ubuntu-latest) + openssl. Trap cleanup.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMG="gocdnext-plugin-buildx:smoke-$$"
NET="buildx-smoke-$$"; DIND="buildx-smoke-dind-$$"
# A pinned representative of the default docker:24 daemon (the k8s runtime wires
# the floating tag docker:24-dind in agent/internal/engine/kubernetes.go).
DIND_IMAGE="docker:24.0.9-dind@sha256:9b17a9f25adf17b88d0a013b4f00160754adf4b07ccbe9986664a49886c2c98e"
# Must match DEFAULT_BUILDKIT_IMAGE in entrypoint.sh — we FROM it so the real
# builder runs the pinned BuildKit, only with our test CA appended.
BUILDKIT="moby/buildkit:v0.34.2@sha256:c5f8862f52499773af85bc07695b2379ef08e8002352b0df2c4864c91d78dabe"
# S3-compatible store for the `cache: bucket` scenario (#335). adobe/s3mock — a
# purpose-built in-memory S3 mock (floating tag, same spirit as registry:2 below;
# minio/minio is not pullable through this CI's Docker Hub mirror). Like MinIO in
# this topology it publishes no bucket.<host> virtual-host DNS, so it answers
# ONLY path-style requests — which the plugin emits only on the opt-in knob,
# making GOCDNEXT_LAYER_CACHE_USE_PATH_STYLE=true load-bearing here. It pre-creates
# the bucket via initialBuckets and ignores request auth, so the AWS creds we pass
# are pure no-leak sentinels: they must NEVER reach the buildkitd argv/Config.Env.
S3MOCK_IMAGE="adobe/s3mock:latest"
S3_AK="gocdnextsmokeak"
S3_SK="SENTINELsmokeSECRETpw8chars"
S3_TOKEN="SENTINELsmokeSESSIONtoken"
BUCKET="bxcache"
W="$(mktemp -d)"
cleanup() {
  docker rm -f "$DIND" >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
  docker image rm -f "$IMG" >/dev/null 2>&1 || true
  rm -rf "$W"
}
trap cleanup EXIT
die() { echo "FAIL: $1"; [ -n "${2:-}" ] && { echo "--- output ---"; echo "$2" | tail -40; }; exit 1; }

H() { docker run --rm --network "$NET" -e DOCKER_HOST=tcp://dind:2375 --entrypoint docker "$IMG" "$@"; }
# curl from INSIDE dind, trusting our CA, against the published registry port.
reg_curl() { docker exec "$DIND" sh -c "docker run --rm -v /certs:/c:ro curlimages/curl:8.11.0 $1"; }

echo "==> build plugin image (same Dockerfile/context as the official build)"
docker build -t "$IMG" "$HERE" >/dev/null

echo "==> docker:24 DinD daemon"
docker network create "$NET" >/dev/null
docker run -d --name "$DIND" --network "$NET" --network-alias dind --privileged \
  -e DOCKER_TLS_CERTDIR= "$DIND_IMAGE" >/dev/null
for _ in $(seq 1 30); do H info >/dev/null 2>&1 && break; sleep 1; done
H info >/dev/null 2>&1 || die "dind daemon never became reachable"

echo "==> dind bridge gateway (how the builder reaches the published registry)"
GIP="$(docker exec "$DIND" docker network inspect bridge --format '{{(index .IPAM.Config 0).Gateway}}')"
[ -n "$GIP" ] || die "could not resolve the dind bridge gateway"
REF="$GIP:5000"

echo "==> mint a registry TLS cert (SAN IP:$GIP)"
openssl req -x509 -newkey rsa:2048 -nodes -keyout "$W/tls.key" -out "$W/tls.crt" \
  -subj "/CN=buildx-smoke-reg" -addext "subjectAltName=IP:$GIP" -days 2 >/dev/null 2>&1 \
  || die "openssl cert generation failed"
docker exec "$DIND" mkdir -p /certs
docker cp "$W/tls.crt" "$DIND:/certs/tls.crt"; docker cp "$W/tls.key" "$DIND:/certs/tls.key"

echo "==> TLS registry inside dind, published on the dind host (:5000)"
docker exec -d "$DIND" sh -c 'docker run -d --name reg -p 5000:5000 \
  -e REGISTRY_HTTP_ADDR=0.0.0.0:5000 \
  -e REGISTRY_HTTP_TLS_CERTIFICATE=/certs/tls.crt -e REGISTRY_HTTP_TLS_KEY=/certs/tls.key \
  -v /certs:/certs:ro registry:2'
for _ in $(seq 1 30); do reg_curl "-sf --cacert /c/tls.crt https://$REF/v2/ >/dev/null 2>&1" && break; sleep 1; done
reg_curl "-sf --cacert /c/tls.crt https://$REF/v2/ >/dev/null 2>&1" || die "registry never became reachable"

echo "==> bake the registry CA into a BuildKit image (FROM the pinned default)"
docker exec "$DIND" mkdir -p /bk
docker cp "$W/tls.crt" "$DIND:/bk/ca.crt"
docker exec "$DIND" sh -c "printf 'FROM %s\nCOPY ca.crt /tmp/ca.crt\nRUN cat /tmp/ca.crt >> /etc/ssl/certs/ca-certificates.crt\n' '$BUILDKIT' > /bk/Dockerfile"
docker exec "$DIND" docker build -t bk-ca:smoke /bk >/dev/null || die "could not build the CA-trusting buildkit image"

echo "==> build context: two-arch-buildable with a RUN layer (cacheable)"
mkdir -p "$W/ctx"
printf 'FROM alpine:3.20\nRUN echo "hi" > /marker\n' > "$W/ctx/Dockerfile"

run_plugin() {
  docker run --rm --network "$NET" -e DOCKER_HOST=tcp://dind:2375 \
    -v "$W/ctx:/workspace" -w /workspace \
    -e PLUGIN_IMAGE="$REF/bx" -e PLUGIN_TAGS="multi" \
    -e PLUGIN_PLATFORMS="linux/amd64,linux/arm64" \
    -e PLUGIN_BUILDKIT_IMAGE="bk-ca:smoke" \
    -e PLUGIN_CACHE="registry" \
    "$IMG" 2>&1
}

echo "==> build #1 — multi-arch build, push, warm the registry cache"
set +e; O1="$(run_plugin)"; R1=$?; set -e
[ "$R1" -eq 0 ] || die "multi-arch build+push failed (rc=$R1)" "$O1"

echo "==> manifest list must contain BOTH amd64 and arm64"
MAN="$(reg_curl "-s --cacert /c/tls.crt -H 'Accept: application/vnd.docker.distribution.manifest.list.v2+json' -H 'Accept: application/vnd.oci.image.index.v1+json' https://$REF/v2/bx/manifests/multi")"
grep -qE '"architecture": ?"amd64"' <<<"$MAN" || die "manifest list missing amd64" "$MAN"
grep -qE '"architecture": ?"arm64"' <<<"$MAN" || die "manifest list missing arm64" "$MAN"

echo "==> build #2 — must reuse the exported cache (CACHED)"
set +e; O2="$(run_plugin)"; R2=$?; set -e
[ "$R2" -eq 0 ] || die "second build failed (rc=$R2)" "$O2"
grep -qi "CACHED" <<<"$O2" || die "second build did not reuse the registry cache" "$O2"

# ---------------------------------------------------------------------------
# #335: cache: bucket → self-managed BuildKit, AWS creds off argv/Config.Env,
# path-style S3 round-trip against MinIO. Everything store-side lives inside
# dind; the self-managed buildkitd (spawned by the plugin via DOCKER_HOST=dind)
# reaches MinIO through the dind bridge gateway, exactly like the registry.
DD() { docker exec "$DIND" docker "$@"; }  # the dind daemon's own docker CLI

echo "==> S3-compatible store (adobe/s3mock) inside dind, published on the dind host (:9090)"
DD run -d --name s3mock -p 9090:9090 \
  -e "initialBuckets=$BUCKET" "$S3MOCK_IMAGE" >/dev/null || die "could not start s3mock"
for _ in $(seq 1 30); do
  DD run --rm "curlimages/curl:8.11.0" -sf "http://$GIP:9090/" >/dev/null 2>&1 && break
  sleep 1
done
DD run --rm "curlimages/curl:8.11.0" -sf "http://$GIP:9090/" >/dev/null 2>&1 \
  || die "s3mock never became ready"
# Create the bucket explicitly (path-style PUT) — the initialBuckets env is
# version-dependent and didn't take on :latest. s3mock ignores request auth.
DD run --rm "curlimages/curl:8.11.0" -sf -X PUT "http://$GIP:9090/$BUCKET" >/dev/null 2>&1 \
  || die "could not create the S3 bucket"
DD run --rm "curlimages/curl:8.11.0" -sf "http://$GIP:9090/$BUCKET/?list-type=2" >/dev/null 2>&1 \
  || die "bucket not reachable after creation"

bucket_env=(
  -e PLUGIN_CACHE="bucket"
  -e GOCDNEXT_LAYER_CACHE_BUCKET="$BUCKET"
  -e GOCDNEXT_LAYER_CACHE_ENDPOINT="http://$GIP:9090"
  -e GOCDNEXT_LAYER_CACHE_REGION="us-east-1"
  -e GOCDNEXT_LAYER_CACHE_USE_PATH_STYLE="true"
  -e AWS_ACCESS_KEY_ID="$S3_AK"
  -e AWS_SECRET_ACCESS_KEY="$S3_SK"
)
no_bk_left() { [ -z "$(DD ps -aq --filter name=gocdnext-buildkitd 2>/dev/null)" ]; }

# --- Run A: no-leak (live inspect) + session token in the creds file + mirror
#     wiring + forced-failure cleanup. A session token makes MinIO auth moot,
#     and `RUN false` fails the build deterministically — the point is to catch
#     the self-managed buildkitd ALIVE, inspect it, then prove teardown.
echo "==> [bucket] Run A: creds off argv/Config.Env + session token + cleanup-on-failure"
no_bk_left || die "a stray gocdnext-buildkitd existed before Run A"
mkdir -p "$W/ctxA"; printf 'FROM alpine:3.20\nRUN sleep 6 && false\n' > "$W/ctxA/Dockerfile"
docker run --rm --network "$NET" -e DOCKER_HOST=tcp://dind:2375 \
  -v "$W/ctxA:/workspace" -w /workspace \
  -e PLUGIN_IMAGE="$REF/bxbucket" -e PLUGIN_TAGS="a" -e PLUGIN_PUSH="false" \
  -e PLUGIN_MIRROR="mirror.invalid" \
  "${bucket_env[@]}" -e AWS_SESSION_TOKEN="$S3_TOKEN" \
  "$IMG" >"$W/runA.log" 2>&1 &
PIDA=$!

bk=""
for _ in $(seq 1 150); do
  bk="$(DD ps --filter name=gocdnext-buildkitd --format '{{.Names}}' 2>/dev/null | head -1 || true)"
  [ -n "$bk" ] && break
  sleep 0.2
done
[ -n "$bk" ] || { cat "$W/runA.log"; die "self-managed buildkitd never appeared"; }

ENVJSON="$(DD inspect "$bk" --format '{{json .Config.Env}}' 2>/dev/null || true)"
ARGVJSON="$(DD inspect "$bk" --format '{{.Path}} {{json .Args}} {{json .Config.Cmd}} {{json .Config.Entrypoint}}' 2>/dev/null || true)"
grep -qF "$S3_SK" <<<"$ENVJSON"  && { kill "$PIDA" 2>/dev/null||true; die "SECRET leaked into buildkitd Config.Env" "$ENVJSON"; }
grep -qF "$S3_TOKEN" <<<"$ENVJSON" && { kill "$PIDA" 2>/dev/null||true; die "SESSION TOKEN leaked into buildkitd Config.Env" "$ENVJSON"; }
grep -qF "$S3_SK" <<<"$ARGVJSON" && { kill "$PIDA" 2>/dev/null||true; die "SECRET leaked onto buildkitd argv" "$ARGVJSON"; }
grep -qF "$S3_TOKEN" <<<"$ARGVJSON" && { kill "$PIDA" 2>/dev/null||true; die "SESSION TOKEN leaked onto buildkitd argv" "$ARGVJSON"; }
grep -q "AWS_SHARED_CREDENTIALS_FILE=/etc/gocdnext/aws-credentials" <<<"$ENVJSON" \
  || die "AWS_SHARED_CREDENTIALS_FILE not set on buildkitd" "$ENVJSON"
grep -q "AWS_REQUEST_CHECKSUM_CALCULATION=when_required" <<<"$ENVJSON" \
  || die "checksum opt-out (non-AWS endpoint) not set on buildkitd" "$ENVJSON"
# The credentials FILE carries the secret + session token, mode 0600.
MODE="$(DD exec "$bk" stat -c '%a' /etc/gocdnext/aws-credentials 2>/dev/null || true)"
[ "$MODE" = "600" ] || die "credentials file is not 0600 (got '${MODE}')"
CREDS="$(DD exec "$bk" cat /etc/gocdnext/aws-credentials 2>/dev/null || true)"
grep -q "aws_secret_access_key = $S3_SK" <<<"$CREDS" || die "secret missing from the credentials file"
grep -q "aws_session_token = $S3_TOKEN" <<<"$CREDS" || die "session token missing from the credentials file"
# Mirror config was copied in and wired via --config.
grep -q "/etc/gocdnext/buildkitd.toml" <<<"$ARGVJSON" || die "buildkitd.toml not wired via --config" "$ARGVJSON"
DD exec "$bk" cat /etc/gocdnext/buildkitd.toml 2>/dev/null | grep -q "mirror.invalid" \
  || die "mirror config not present in the staged buildkitd.toml"

set +e; wait "$PIDA"; RA=$?; set -e
[ "$RA" -ne 0 ] || { cat "$W/runA.log"; die "Run A was supposed to fail (RUN false) but exited 0"; }
no_bk_left || die "buildkitd was NOT cleaned up after a forced failure"
grep -qF "$S3_SK" "$W/runA.log" && die "secret leaked into the plugin stdout"
grep -qF "$S3_TOKEN" "$W/runA.log" && die "session token leaked into the plugin stdout"
echo "OK: [bucket] creds off argv+Config.Env, session token staged in 0600 file, cleanup on failure"

# --- Run B: real path-style S3 cache round-trip against MinIO (success path).
echo "==> [bucket] Run B: path-style S3 cache export (#1) then CACHED (#2)"
mkdir -p "$W/ctxB"; printf 'FROM alpine:3.20\nRUN echo bucketcache > /m\n' > "$W/ctxB/Dockerfile"
run_bucket_build() {
  docker run --rm --network "$NET" -e DOCKER_HOST=tcp://dind:2375 \
    -v "$W/ctxB:/workspace" -w /workspace \
    -e PLUGIN_IMAGE="$REF/bxbucket" -e PLUGIN_TAGS="b" -e PLUGIN_PUSH="false" \
    "${bucket_env[@]}" \
    "$IMG" 2>&1
}
set +e; OB1="$(run_bucket_build)"; RB1=$?; set -e
[ "$RB1" -eq 0 ] || die "bucket build #1 failed (rc=$RB1)" "$OB1"
no_bk_left || die "buildkitd was NOT cleaned up after a successful build #1"
# The export must have actually written cache objects to the bucket (path-style
# ListObjectsV2 — proof the opt-in style reached a real store, not just the argv).
LISTXML="$(DD run --rm "curlimages/curl:8.11.0" -s "http://$GIP:9090/$BUCKET/?list-type=2" 2>/dev/null || true)"
grep -q "<Key>" <<<"$LISTXML" || die "build #1 exported nothing to the bucket (no objects listed)" "$OB1
--- list ---
$LISTXML"

set +e; OB2="$(run_bucket_build)"; RB2=$?; set -e
[ "$RB2" -eq 0 ] || die "bucket build #2 failed (rc=$RB2)" "$OB2"
grep -qi "CACHED" <<<"$OB2" || die "build #2 did not import the path-style S3 cache (no CACHED)" "$OB2"
no_bk_left || die "buildkitd was NOT cleaned up after a successful build #2"
echo "OK: [bucket] path-style S3 round-trip — build #1 exported, build #2 CACHED, cleanup on success"

echo "PASS: buildx smoke"
