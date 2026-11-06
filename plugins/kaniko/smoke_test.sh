#!/usr/bin/env bash
# Real smoke for the kaniko plugin (community fork osscontainertools/kaniko).
# The entrypoint execs /kaniko/executor by ABSOLUTE path, so it can't be mocked
# on a host PATH — everything runs against the built image, in two phases:
#
#   Phase 1 — mock executor (mounted over /kaniko/executor): the PER-ARGUMENT
#     argv the wrapper builds (so a space-bearing build-arg stays one arg), and
#     the generated /kaniko/.docker/config.json — correct base64 auth, mode 600,
#     password off argv/log.
#   Phase 2 — real executor against a throwaway TLS registry: a `--no-push` build
#     with a spaced build-arg, a real push + manifest read-back, and a real
#     `cache=true` build that stores AND reuses a RUN layer.
#
# Convention-named so plugins.yml runs it. Needs docker + openssl.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMG="gocdnext-plugin-kaniko:smoke-$$"
NET="kaniko-smoke-$$"; REG="kaniko-smoke-reg-$$"
W="$(mktemp -d)"
cleanup() {
  docker rm -f "$REG" >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
  docker image rm -f "$IMG" >/dev/null 2>&1 || true
  rm -rf "$W"
}
trap cleanup EXIT
die() { echo "FAIL: $1"; [ -n "${2:-}" ] && { echo "--- output ---"; echo "$2"; }; exit 1; }

echo "==> build (same Dockerfile/context as the official build)"
docker build -t "$IMG" "$HERE" >/dev/null

# A cacheable context: a RUN layer (FROM scratch has none, which hid the broken
# cache repo). ARG lets us prove a spaced build-arg survives.
mkdir -p "$W/ctx"
cat >"$W/ctx/Dockerfile" <<'EOF'
FROM alpine:3.20
ARG MSG=default
RUN echo "built:${MSG}" > /marker
EOF

# ---- Phase 1: mock executor -------------------------------------------------
mkdir -p "$W/mock" "$W/out"
cat >"$W/mock/executor" <<'EOF'
#!/busybox/sh
: > /out/rec
for a in "$@"; do echo "ARG:$a" >> /out/rec; done
{ echo "---CONFIG---"; cat /kaniko/.docker/config.json 2>/dev/null || echo NO-CONFIG
  echo "---MODE---"; /busybox/stat -c '%a %n' /kaniko/.docker /kaniko/.docker/config.json 2>/dev/null || echo NO-STAT
} >> /out/rec
exit 0
EOF
chmod +x "$W/mock/executor"
mock_run() { # mock_run <out> <docker -e flags...>
  local of="$1"; shift; : >"$W/out/rec"
  set +e
  MOCK_OUT="$(docker run --rm -v "$W/ctx:/workspace" -w /workspace \
    -v "$W/mock/executor:/kaniko/executor:ro" -v "$W/out:/out" "$@" "$IMG" 2>&1)"; MOCK_RC=$?
  set -e
  cp "$W/out/rec" "$of" 2>/dev/null || true
}

# 1a. PLUGIN_IMAGE required → exit 2, executor not called.
mock_run "$W/out/1a"
[ "$MOCK_RC" -eq 2 ] || die "missing PLUGIN_IMAGE should exit 2 (rc=$MOCK_RC)" "$MOCK_OUT"
[ -s "$W/out/1a" ] && die "executor must not run without PLUGIN_IMAGE"

# 1b. argv is per-argument correct, and a build-arg VALUE with a space stays ONE
#     argument (the old temp-file word-split corrupted "MSG=hello world").
mock_run "$W/out/1b" -e PLUGIN_IMAGE=reg/app:1 -e PLUGIN_PUSH=false -e PLUGIN_BUILD_ARGS="MSG=hello world,EXTRA=1"
grep -qx "ARG:--destination=reg/app:1" "$W/out/1b" || die "argv missing --destination" "$(cat "$W/out/1b")"
grep -qx "ARG:--no-push" "$W/out/1b" || die "argv missing --no-push" "$(cat "$W/out/1b")"
grep -qx "ARG:MSG=hello world" "$W/out/1b" || die "spaced build-arg was split (not one argument)" "$(cat "$W/out/1b")"
grep -qx "ARG:EXTRA=1" "$W/out/1b" || die "argv missing EXTRA build-arg" "$(cat "$W/out/1b")"
[ "$(grep -cx 'ARG:--build-arg' "$W/out/1b")" = "2" ] || die "expected exactly two --build-arg flags" "$(cat "$W/out/1b")"

# 1c. credentials → config.json with correct base64 auth, mode 0600 (dir 0700),
#     password never on argv or in the log.
PW="sup3r-s3cret-$$"
mock_run "$W/out/1c" -e PLUGIN_IMAGE=reg.example/app:1 -e PLUGIN_REGISTRY=reg.example \
  -e PLUGIN_USERNAME=robot -e PLUGIN_PASSWORD="$PW"
want="$(printf '%s:%s' robot "$PW" | base64 -w0)"
grep -q "\"auth\":\"$want\"" "$W/out/1c" || die "config.json missing correct base64 auth" "$(cat "$W/out/1c")"
grep -qE "^600 /kaniko/\.docker/config\.json$" "$W/out/1c" || die "config.json is not mode 0600" "$(cat "$W/out/1c")"
grep -qE "^700 /kaniko/\.docker$" "$W/out/1c" || die "/kaniko/.docker is not mode 0700" "$(cat "$W/out/1c")"
grep -q "$PW" "$W/out/1c" && die "plaintext password leaked into argv/config"
grep -q "$PW" <<<"$MOCK_OUT" && die "plaintext password leaked into the plugin log"

# ---- Phase 2: real executor + throwaway TLS registry ------------------------
mkdir -p "$W/certs"
openssl req -x509 -newkey rsa:2048 -nodes -keyout "$W/certs/tls.key" -out "$W/certs/tls.crt" \
  -subj "/CN=reg" -addext "subjectAltName=DNS:reg" -days 2 >/dev/null 2>&1
# kaniko must trust BOTH our registry (push/cache) AND Docker Hub (pull alpine):
# combine the image's own CA bundle with our self-signed cert.
docker run --rm --entrypoint /busybox/sh "$IMG" -c 'cat /kaniko/ssl/certs/ca-certificates.crt' > "$W/certs/bundle.crt"
cat "$W/certs/tls.crt" >> "$W/certs/bundle.crt"

docker network create "$NET" >/dev/null
docker run -d --name "$REG" --network "$NET" --network-alias reg \
  -e REGISTRY_HTTP_ADDR=0.0.0.0:5000 \
  -e REGISTRY_HTTP_TLS_CERTIFICATE=/certs/tls.crt -e REGISTRY_HTTP_TLS_KEY=/certs/tls.key \
  -v "$W/certs:/certs:ro" registry:2 >/dev/null
for _ in $(seq 1 20); do docker run --rm --network "$NET" -v "$W/certs/tls.crt:/ca:ro" curlimages/curl:8.11.0 -sf --cacert /ca https://reg:5000/v2/ >/dev/null 2>&1 && break; sleep 1; done

build() { # build <docker -e flags...>
  set +e
  B_OUT="$(docker run --rm --network "$NET" -v "$W/ctx:/workspace" -w /workspace \
    -v "$W/certs/bundle.crt:/kaniko/ssl/certs/ca-certificates.crt:ro" "$@" "$IMG" 2>&1)"; B_RC=$?
  set -e
}

# 2a. --no-push build with a SPACED build-arg — real end-to-end exercise of the
#     here-doc build-args path (and the alpine pull via the combined CA).
build -e PLUGIN_IMAGE=reg:5000/smoke:nopush -e PLUGIN_PUSH=false -e PLUGIN_BUILD_ARGS="MSG=hello world"
[ "$B_RC" -eq 0 ] || die "--no-push build with a spaced build-arg should succeed (rc=$B_RC)" "$B_OUT"

# 2b. real push + read the manifest back (config + layers, not just the tag).
build -e PLUGIN_IMAGE=reg:5000/smoke:1
[ "$B_RC" -eq 0 ] || die "push build should succeed (rc=$B_RC)" "$B_OUT"
MAN="$(docker run --rm --network "$NET" -v "$W/certs/tls.crt:/ca:ro" curlimages/curl:8.11.0 -s --cacert /ca \
  -H 'Accept: application/vnd.oci.image.manifest.v1+json' \
  -H 'Accept: application/vnd.docker.distribution.manifest.v2+json' \
  https://reg:5000/v2/smoke/manifests/1 2>&1)"
grep -q '"config"' <<<"$MAN" || die "pushed manifest has no config descriptor" "$MAN"
grep -q '"layers"' <<<"$MAN" || die "pushed manifest has no layers" "$MAN"

# 2c. cache=true must actually STORE then REUSE the RUN layer. First build warms
#     the cache; the second must report a cache hit — and neither may fail to
#     upload the cache layer (the silently-broken cache-repo bug).
build -e PLUGIN_IMAGE=reg:5000/smoke:2 -e PLUGIN_CACHE=true
[ "$B_RC" -eq 0 ] || die "cache=true first build should succeed (rc=$B_RC)" "$B_OUT"
if grep -qi "Error uploading layer to cache" <<<"$B_OUT"; then
  die "cache layer failed to upload — cache-repo is malformed" "$B_OUT"
fi
build -e PLUGIN_IMAGE=reg:5000/smoke:2 -e PLUGIN_CACHE=true
[ "$B_RC" -eq 0 ] || die "cache=true second build should succeed (rc=$B_RC)" "$B_OUT"
grep -qi "Using caching version of cmd\|Found cached layer" <<<"$B_OUT" \
  || die "second cache=true build did not reuse the cached RUN layer" "$B_OUT"
if grep -qi "Error uploading layer to cache" <<<"$B_OUT"; then
  die "cache layer failed to upload on the second build" "$B_OUT"
fi

echo "PASS: kaniko smoke"
