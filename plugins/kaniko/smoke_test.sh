#!/usr/bin/env bash
# Real smoke for the kaniko plugin (community fork osscontainertools/kaniko).
# The entrypoint execs /kaniko/executor by ABSOLUTE path, so it can't be mocked
# on a host PATH — everything runs against the built image here, in two phases:
#
#   Phase 1 — mock executor (mounted over /kaniko/executor): the argv the wrapper
#     builds, the generated /kaniko/.docker/config.json, and the credential never
#     reaching argv/log. No real build.
#   Phase 2 — real executor against a throwaway TLS registry: a `--no-push` build
#     WITH a build-arg (captures the missing-/tmp break the fork image had), a
#     real push + read-back, and a cache=true push + second build.
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

# ---- Phase 1: mock executor -------------------------------------------------
mkdir -p "$W/mock" "$W/out"
cat >"$W/mock/executor" <<'EOF'
#!/busybox/sh
{ echo "ARGV=$*"; echo "---CONFIG---"; cat /kaniko/.docker/config.json 2>/dev/null || echo "NO-CONFIG"; } > /out/rec
exit 0
EOF
chmod +x "$W/mock/executor"
mkctx() { mkdir -p "$W/ctx"; printf 'FROM scratch\nARG MSG=default\nCOPY payload /payload\n' >"$W/ctx/Dockerfile"; printf 'x\n' >"$W/ctx/payload"; }
mkctx
mock_run() { # mock_run <out-file> <docker -e flags...>
  local of="$1"; shift; : >"$W/out/rec"
  set +e
  MOCK_RC_OUT="$(docker run --rm -v "$W/ctx:/workspace" -w /workspace \
    -v "$W/mock/executor:/kaniko/executor:ro" -v "$W/out:/out" "$@" "$IMG" 2>&1)"; MOCK_RC=$?
  set -e
  cp "$W/out/rec" "$of" 2>/dev/null || true
}

# 1a. PLUGIN_IMAGE required → exit 2, executor not called.
mock_run "$W/out/1a" ; true   # no IMAGE
[ "$MOCK_RC" -eq 2 ] || die "missing PLUGIN_IMAGE should exit 2 (rc=$MOCK_RC)" "$MOCK_RC_OUT"
[ -s "$W/out/1a" ] && die "executor must not run without PLUGIN_IMAGE"

# 1b. argv shape: --no-push + build-arg collected onto the executor command line.
mock_run "$W/out/1b" -e PLUGIN_IMAGE=reg/app:1 -e PLUGIN_PUSH=false -e PLUGIN_BUILD_ARGS="MSG=hi,EXTRA=1"
argv="$(grep '^ARGV=' "$W/out/1b")"
grep -q -- "--dockerfile=./Dockerfile" <<<"$argv" || die "argv missing --dockerfile" "$argv"
grep -q -- "--destination=reg/app:1" <<<"$argv" || die "argv missing --destination" "$argv"
grep -q -- "--no-push" <<<"$argv" || die "argv missing --no-push" "$argv"
grep -q -- "--build-arg MSG=hi" <<<"$argv" || die "argv missing build-arg MSG" "$argv"
grep -q -- "--build-arg EXTRA=1" <<<"$argv" || die "argv missing build-arg EXTRA" "$argv"

# 1c. credentials: username/password -> /kaniko/.docker/config.json with base64
#     auth; the password never appears on argv or in the output.
PW="sup3r-s3cret-$$"
mock_run "$W/out/1c" -e PLUGIN_IMAGE=reg.example/app:1 -e PLUGIN_REGISTRY=reg.example \
  -e PLUGIN_USERNAME=robot -e PLUGIN_PASSWORD="$PW"
want="$(printf '%s:%s' robot "$PW" | base64 -w0)"
grep -q "\"auth\":\"$want\"" "$W/out/1c" || die "config.json missing correct base64 auth" "$(cat "$W/out/1c")"
grep -q "reg.example" "$W/out/1c" || die "config.json missing registry host" "$(cat "$W/out/1c")"
grep -q "$PW" "$W/out/1c" && die "plaintext password leaked into argv/config" "$(cat "$W/out/1c")"
grep -q "$PW" <<<"$MOCK_RC_OUT" && die "plaintext password leaked into the plugin log"

# ---- Phase 2: real executor + throwaway TLS registry ------------------------
mkdir -p "$W/certs"
openssl req -x509 -newkey rsa:2048 -nodes -keyout "$W/certs/tls.key" -out "$W/certs/tls.crt" \
  -subj "/CN=reg" -addext "subjectAltName=DNS:reg" -days 2 >/dev/null 2>&1
docker network create "$NET" >/dev/null
docker run -d --name "$REG" --network "$NET" --network-alias reg \
  -e REGISTRY_HTTP_ADDR=0.0.0.0:5000 \
  -e REGISTRY_HTTP_TLS_CERTIFICATE=/certs/tls.crt -e REGISTRY_HTTP_TLS_KEY=/certs/tls.key \
  -v "$W/certs:/certs:ro" registry:2 >/dev/null
for _ in $(seq 1 20); do docker run --rm --network "$NET" -v "$W/certs/tls.crt:/ca:ro" curlimages/curl:8.11.0 -sf --cacert /ca https://reg:5000/v2/ >/dev/null 2>&1 && break; sleep 1; done

# build+push helper through the wrapper, trusting our CA as kaniko's bundle
build() { # build <docker -e flags...>
  set +e
  B_OUT="$(docker run --rm --network "$NET" -v "$W/ctx:/workspace" -w /workspace \
    -v "$W/certs/tls.crt:/kaniko/ssl/certs/ca-certificates.crt:ro" "$@" "$IMG" 2>&1)"; B_RC=$?
  set -e
}
readable() { # readable <tag> -> 0 if the pushed tag is listed
  docker run --rm --network "$NET" -v "$W/certs/tls.crt:/ca:ro" curlimages/curl:8.11.0 \
    -s --cacert /ca "https://reg:5000/v2/smoke/tags/list" 2>/dev/null | grep -q "\"$1\""
}

# 2a. --no-push build WITH a build-arg — exercises the /tmp build-args path (the
#     fork image had no /tmp; a regression here fails before kaniko even runs).
build -e PLUGIN_IMAGE=reg:5000/smoke:nopush -e PLUGIN_PUSH=false -e PLUGIN_BUILD_ARGS="MSG=hello"
[ "$B_RC" -eq 0 ] || die "--no-push build with build-arg should succeed (rc=$B_RC)" "$B_OUT"
grep -qiE "/tmp/kaniko-build-args|no such file" <<<"$B_OUT" && die "build-arg path hit a /tmp error — the /tmp fix regressed" "$B_OUT"

# 2b. real push + read it back.
build -e PLUGIN_IMAGE=reg:5000/smoke:1 -e PLUGIN_CONTEXT=.
[ "$B_RC" -eq 0 ] || die "push build should succeed (rc=$B_RC)" "$B_OUT"
readable 1 || die "pushed image reg:5000/smoke:1 is not readable from the registry" "$B_OUT"

# 2c. cache=true: a push + a second build (populates then reuses the cache repo).
build -e PLUGIN_IMAGE=reg:5000/smoke:2 -e PLUGIN_CACHE=true -e PLUGIN_CONTEXT=.
[ "$B_RC" -eq 0 ] || die "cache=true first build should succeed (rc=$B_RC)" "$B_OUT"
build -e PLUGIN_IMAGE=reg:5000/smoke:2 -e PLUGIN_CACHE=true -e PLUGIN_CONTEXT=.
[ "$B_RC" -eq 0 ] || die "cache=true second build should succeed (rc=$B_RC)" "$B_OUT"
readable 2 || die "cache=true pushed image reg:5000/smoke:2 is not readable" "$B_OUT"

echo "PASS: kaniko smoke"
