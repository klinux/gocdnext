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
DIND_IMAGE="docker:24.0.9-dind@sha256:9b17a9f25adf17b88d0a013b4f00160754adf4b07ccbe9986664a49886c2c98e"
# Must match DEFAULT_BUILDKIT_IMAGE in entrypoint.sh — we FROM it so the real
# builder runs the pinned BuildKit, only with our test CA appended.
BUILDKIT="moby/buildkit:v0.34.2@sha256:c5f8862f52499773af85bc07695b2379ef08e8002352b0df2c4864c91d78dabe"
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

echo "PASS: buildx smoke"
