#!/usr/bin/env bash
# Real smoke for the docker-push plugin: login + retag + push WITHOUT building.
# Stands up a throwaway docker:24 DinD (the gocdnext k8s runtime default) + an
# insecure sibling registry, and proves END TO END:
#   - a pre-loaded image is retagged + pushed under EVERY requested tag, and
#     each tag is a faithful copy of the SOURCE (a marker baked into the source
#     round-trips — proving a retag, not a rebuild),
#   - PLUGIN_PULL=true really pulls a source that is only in the registry
#     before tagging + pushing it on to a second repo.
#
# Convention-named so plugins.yml runs it as a PRE-PUBLISH gate. Needs docker
# (privileged DinD — fine on ubuntu-latest). Throwaway network + trap cleanup.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMG="gocdnext-plugin-docker-push:smoke-$$"
NET="dpush-smoke-$$"; DIND="dpush-smoke-dind-$$"; REG="dpush-smoke-reg-$$"
DIND_IMAGE="docker:24.0.9-dind@sha256:9b17a9f25adf17b88d0a013b4f00160754adf4b07ccbe9986664a49886c2c98e"
MARKER="promoted-marker-$$"
W="$(mktemp -d)"
cleanup() {
  docker rm -f "$DIND" "$REG" >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
  docker image rm -f "$IMG" >/dev/null 2>&1 || true
  rm -rf "$W"
}
trap cleanup EXIT
die() { echo "FAIL: $1"; [ -n "${2:-}" ] && { echo "--- output ---"; echo "$2" | tail -40; }; exit 1; }

dkr() { docker run --rm --network "$NET" -e DOCKER_HOST=tcp://dind:2375 --entrypoint docker "$IMG" "$@"; }
curlc() { docker run --rm --network "$NET" curlimages/curl:8.11.0 "$@"; }
plugin() { # plugin <-e ...>: run the docker-push entrypoint against the daemon
  docker run --rm --network "$NET" -e DOCKER_HOST=tcp://dind:2375 "$@" "$IMG"
}

echo "==> build plugin image (same Dockerfile/context as the official build)"
docker build -t "$IMG" "$HERE" >/dev/null

echo "==> network + insecure registry + docker:24 DinD daemon"
docker network create "$NET" >/dev/null
docker run -d --name "$REG" --network "$NET" --network-alias reg registry:2 >/dev/null
docker run -d --name "$DIND" --network "$NET" --network-alias dind --privileged \
  -e DOCKER_TLS_CERTDIR= "$DIND_IMAGE" --insecure-registry reg:5000 >/dev/null
for _ in $(seq 1 30); do dkr info >/dev/null 2>&1 && break; sleep 1; done
dkr info >/dev/null 2>&1 || die "dind daemon never became reachable"
for _ in $(seq 1 30); do curlc -sf http://reg:5000/v2/ >/dev/null 2>&1 && break; sleep 1; done
curlc -sf http://reg:5000/v2/ >/dev/null 2>&1 || die "registry never became reachable"

echo "==> pre-load a source image in the daemon (marker baked in, NO registry)"
mkdir -p "$W/src"
cat >"$W/src/Dockerfile" <<EOF
FROM alpine:3.20
RUN echo "${MARKER}" > /id
EOF
docker run --rm --network "$NET" -e DOCKER_HOST=tcp://dind:2375 \
  -v "$W/src:/c" -w /c --entrypoint docker "$IMG" build -t local/src:orig . >/dev/null \
  || die "could not pre-load source image"

# ---- Phase 1: retag local source → two tags on a target repo, push ----------
echo "==> phase 1: retag local/src:orig → reg:5000/promoted {r1,r2}, push"
set +e
OUT="$(plugin -e PLUGIN_SOURCE=local/src:orig -e PLUGIN_TARGET=reg:5000/promoted \
  -e PLUGIN_TAGS="r1 r2" 2>&1)"; RC=$?
set -e
[ "$RC" -eq 0 ] || die "retag+push failed (rc=$RC)" "$OUT"

echo "==> both tags present in the registry"
TAGS="$(curlc -s http://reg:5000/v2/promoted/tags/list)"
for t in r1 r2; do grep -q "\"$t\"" <<<"$TAGS" || die "tag '$t' missing" "$TAGS"; done

echo "==> pushed image is a faithful copy of the source (marker round-trips)"
# Drop the local tag so the pull below truly resolves from the registry; keep
# pull chatter (stderr) OUT of the captured marker (stdout only).
dkr image rm -f reg:5000/promoted:r1 >/dev/null 2>&1 || true
dkr pull reg:5000/promoted:r1 >/dev/null 2>&1 || die "could not pull back promoted:r1" "$OUT"
GOT="$(dkr run --rm reg:5000/promoted:r1 cat /id 2>/dev/null)"
[ "$GOT" = "$MARKER" ] || die "pushed image is not the source (got '$GOT', want '$MARKER')" "$OUT"

# ---- Phase 2: PLUGIN_PULL=true — source lives only in the registry ----------
echo "==> phase 2: PLUGIN_PULL=true pulls reg:5000/promoted:r2, pushes reg:5000/promoted2:p1"
# Remove every local copy so the plugin MUST pull to have anything to tag.
dkr image rm -f reg:5000/promoted:r2 local/src:orig >/dev/null 2>&1 || true
set +e
OUT2="$(plugin -e PLUGIN_SOURCE=reg:5000/promoted:r2 -e PLUGIN_PULL=true \
  -e PLUGIN_TARGET=reg:5000/promoted2 -e PLUGIN_TAGS="p1" 2>&1)"; RC2=$?
set -e
[ "$RC2" -eq 0 ] || die "pull+retag+push failed (rc=$RC2)" "$OUT2"
grep -qi "docker pull reg:5000/promoted:r2\|Pull complete\|Already exists\|Downloaded\|Status: Image is up to date" <<<"$OUT2" \
  || die "PLUGIN_PULL=true did not pull the source" "$OUT2"
TAGS2="$(curlc -s http://reg:5000/v2/promoted2/tags/list)"
grep -q '"p1"' <<<"$TAGS2" || die "promoted2:p1 missing from the registry" "$TAGS2"
dkr image rm -f reg:5000/promoted2:p1 >/dev/null 2>&1 || true
dkr pull reg:5000/promoted2:p1 >/dev/null 2>&1 || die "could not pull back promoted2:p1" "$OUT2"
GOT2="$(dkr run --rm reg:5000/promoted2:p1 cat /id 2>/dev/null)"
[ "$GOT2" = "$MARKER" ] || die "promoted-on image is not the source (got '$GOT2')" "$OUT2"

echo "PASS: docker-push smoke"
