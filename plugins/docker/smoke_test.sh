#!/usr/bin/env bash
# Real smoke for the docker plugin. The entrypoint runs `docker build` +
# `docker push` against whatever daemon DOCKER_HOST points at, so we stand up a
# throwaway DinD daemon — pinned to the same docker:24 the gocdnext Kubernetes
# runtime defaults to (agent/internal/engine/kubernetes.go) — plus an insecure
# sibling registry on a private network, and prove END TO END:
#   - the 29 CLI in the plugin image actually drives the docker:24 daemon
#     (cross-version API negotiation the base bump depends on),
#   - a build-arg really reaches the built image (read back from the image's
#     config LABEL on the daemon, not merely "the flag was passed"), and a
#     VALUE containing a space stays one argument,
#   - every requested tag builds and pushes,
#   - the pushed image is really in the registry (manifest config + layers)
#     under all those tags.
#
# Convention-named so plugins.yml runs it as a PRE-PUBLISH gate. Needs docker
# (privileged DinD — fine on the ubuntu-latest runner). Throwaway network +
# trap cleanup, independent of the publishing build.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMG="gocdnext-plugin-docker:smoke-$$"
NET="docker-smoke-$$"; DIND="docker-smoke-dind-$$"; REG="docker-smoke-reg-$$"
# Same digest the gocdnext k8s runtime pins for the DinD sidecar.
DIND_IMAGE="docker:24.0.9-dind@sha256:9b17a9f25adf17b88d0a013b4f00160754adf4b07ccbe9986664a49886c2c98e"
W="$(mktemp -d)"
cleanup() {
  docker rm -f "$DIND" "$REG" >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
  docker image rm -f "$IMG" >/dev/null 2>&1 || true
  rm -rf "$W"
}
trap cleanup EXIT
die() { echo "FAIL: $1"; [ -n "${2:-}" ] && { echo "--- output ---"; echo "$2" | tail -40; }; exit 1; }

# Run the docker CLI from inside the plugin image against the DinD daemon — the
# exact 29-CLI/24-daemon pairing the plugin ships, used here for readback.
dkr() { docker run --rm --network "$NET" -e DOCKER_HOST=tcp://dind:2375 --entrypoint docker "$IMG" "$@"; }
curlc() { docker run --rm --network "$NET" curlimages/curl:8.11.0 "$@"; }

echo "==> build plugin image (same Dockerfile/context as the official build)"
docker build -t "$IMG" "$HERE" >/dev/null

echo "==> network + insecure registry + docker:24 DinD daemon"
docker network create "$NET" >/dev/null
docker run -d --name "$REG" --network "$NET" --network-alias reg registry:2 >/dev/null
# reg is a sibling on the same user network, so the daemon resolves it by name;
# --insecure-registry lets the daemon push to it over plain HTTP.
docker run -d --name "$DIND" --network "$NET" --network-alias dind --privileged \
  -e DOCKER_TLS_CERTDIR= "$DIND_IMAGE" --insecure-registry reg:5000 >/dev/null

echo "==> wait for daemon + registry"
for _ in $(seq 1 30); do dkr info >/dev/null 2>&1 && break; sleep 1; done
dkr info >/dev/null 2>&1 || die "dind daemon never became reachable"
for _ in $(seq 1 30); do curlc -sf http://reg:5000/v2/ >/dev/null 2>&1 && break; sleep 1; done
curlc -sf http://reg:5000/v2/ >/dev/null 2>&1 || die "registry never became reachable"

echo "==> build context: a build-arg flows into an image LABEL + a RUN layer"
mkdir -p "$W/ctx"
cat >"$W/ctx/Dockerfile" <<'EOF'
FROM alpine:3.20
ARG BUILD_MSG=unset
LABEL gocdnext.smoke.msg="${BUILD_MSG}"
RUN echo "built:${BUILD_MSG}" > /marker
EOF

echo "==> run the plugin: three tags + a SPACED build-arg, build + push"
set +e
OUT="$(docker run --rm --network "$NET" -e DOCKER_HOST=tcp://dind:2375 \
  -v "$W/ctx:/workspace" -w /workspace \
  -e PLUGIN_IMAGE=reg:5000/smoke -e PLUGIN_TAGS="v1 v2 latest" \
  -e PLUGIN_BUILD_ARGS="BUILD_MSG=hello world" \
  "$IMG" 2>&1)"; RC=$?
set -e
[ "$RC" -eq 0 ] || die "plugin build+push failed (rc=$RC)" "$OUT"

echo "==> build-arg reached the IMAGE (read the LABEL back off the daemon)"
LBL="$(dkr inspect reg:5000/smoke:v1 --format '{{index .Config.Labels "gocdnext.smoke.msg"}}' 2>&1)"
[ "$LBL" = "hello world" ] || die "build-arg not applied to the image (LABEL='$LBL'); a spaced value may have been split" "$OUT"

echo "==> all three tags present in the registry"
TAGS="$(curlc -s http://reg:5000/v2/smoke/tags/list)"
for t in v1 v2 latest; do
  grep -q "\"$t\"" <<<"$TAGS" || die "tag '$t' missing from the registry" "$TAGS"
done

echo "==> pushed manifest has a config descriptor + layers"
MAN="$(curlc -s \
  -H 'Accept: application/vnd.docker.distribution.manifest.v2+json' \
  -H 'Accept: application/vnd.oci.image.manifest.v1+json' \
  http://reg:5000/v2/smoke/manifests/v1)"
grep -q '"config"' <<<"$MAN" || die "pushed manifest has no config descriptor" "$MAN"
grep -q '"layers"' <<<"$MAN" || die "pushed manifest has no layers" "$MAN"

echo "PASS: docker smoke"
