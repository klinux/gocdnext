#!/usr/bin/env bash
# Real smoke for the kustomize plugin's IMAGE. Runs the PINNED binaries end to
# end, fully OFFLINE (--network none) — `build` renders without a cluster. Proves
# the bump (kustomize 5.4.3 -> 5.8.3, kubectl v1.31.1 -> v1.37.2, alpine 3.24.4,
# checksum-verified downloads):
#   - kustomize is v5.8.3 and kubectl is v1.37.2;
#   - a missing path fails fast;
#   - `build` renders a kustomization (namePrefix applied);
#   - `images:` rewrites an image tag in the rendered output;
#   - `envsubst:` substitutes a ${VAR} in the manifests (gettext present).
#
# Convention-named so plugins.yml runs it as a PRE-PUBLISH gate. Needs docker.
# Built --no-cache to match the no_cache publish build. Trap cleanup, fail-closed.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMG="gocdnext-plugin-kustomize:smoke-$$"
W="$(mktemp -d)"
SMOKE_OK=0
cleanup() {
  # envsubst rewrites workspace files in place (root-owned); clear from the image.
  if docker image inspect "$IMG" >/dev/null 2>&1; then
    docker run --rm --entrypoint sh -v "$W:/w" "$IMG" \
      -c 'rm -rf /w/* /w/.[!.]* /w/..?* 2>/dev/null || true' >/dev/null 2>&1 || true
  fi
  docker image rm -f "$IMG" >/dev/null 2>&1 || true
  rm -rf "$W" 2>/dev/null || true
  leftover=0
  [ -d "$W" ] && { echo "WARN: cleanup could not remove ${W}" >&2; leftover=1; }
  docker image inspect "$IMG" >/dev/null 2>&1 && { echo "WARN: smoke image not removed" >&2; leftover=1; }
  [ "$leftover" = 1 ] && [ "$SMOKE_OK" = 1 ] && { echo "FAIL: smoke passed but left state behind" >&2; exit 1; }
  return 0
}
trap cleanup EXIT
die() { echo "FAIL: $1"; [ -n "${2:-}" ] && { echo "--- output ---"; echo "$2" | tail -40; }; exit 1; }

mkoverlay() { # mkoverlay <dir>
  mkdir -p "$1"
  cat >"$1/kustomization.yaml" <<'YAML'
resources: [deployment.yaml]
namePrefix: smoke-
YAML
  cat >"$1/deployment.yaml" <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata: {name: app}
spec:
  template:
    spec:
      containers:
        - name: app
          image: original:v1
          env:
            - name: TOKEN
              value: "${MYVAR}"
YAML
}
run() { # run <subdir> <extra -e...> : sets RC+OUT, offline
  set +e
  OUT="$(docker run --rm --network none -v "$W:/workspace" -w /workspace "${@:2}" "$IMG" 2>&1)"; RC=$?
  set -e
}

echo "==> build plugin image (--no-cache, same Dockerfile/context as the official build)"
docker build --no-cache -t "$IMG" "$HERE" >/dev/null

echo "==> kustomize v5.8.3 + kubectl v1.37.2"
OUT="$(docker run --rm --network none --entrypoint kustomize "$IMG" version 2>&1)" || die "kustomize version failed" "$OUT"
grep -q 'v5.8.3' <<<"$OUT" || die "kustomize is not v5.8.3 (got: ${OUT})" "$OUT"
OUT="$(docker run --rm --network none --entrypoint kubectl "$IMG" version --client 2>&1)" || die "kubectl version failed" "$OUT"
grep -q 'v1.37.2' <<<"$OUT" || die "kubectl is not v1.37.2 (got: ${OUT})" "$OUT"

echo "==> missing path fails fast"
run . -e PLUGIN_PATH="nope-not-a-dir"
[ "$RC" -ne 0 ] || die "missing-path: expected a non-zero exit, got 0" "$OUT"

echo "==> build renders the kustomization (namePrefix applied)"
mkoverlay "$W/base"
run . -e PLUGIN_PATH="base" -e PLUGIN_ACTION="build"
[ "$RC" -eq 0 ] || die "build: expected exit 0, got $RC" "$OUT"
grep -q 'name: smoke-app' <<<"$OUT" || die "build: namePrefix not applied — kustomize didn't render" "$OUT"
grep -q 'image: original:v1' <<<"$OUT" || die "build: image not rendered" "$OUT"

echo "==> images override rewrites the image tag"
mkoverlay "$W/imgs"
run . -e PLUGIN_PATH="imgs" -e PLUGIN_ACTION="build" -e PLUGIN_IMAGES="original=myimg:v2"
[ "$RC" -eq 0 ] || die "images: expected exit 0, got $RC" "$OUT"
grep -q 'image: myimg:v2' <<<"$OUT" || die "images: override not applied in the rendered output" "$OUT"

echo "==> envsubst substitutes a \${VAR} (gettext present)"
mkoverlay "$W/sub"
run . -e PLUGIN_PATH="sub" -e PLUGIN_ACTION="build" -e PLUGIN_ENVSUBST="MYVAR" -e MYVAR="envsubst-ok-$$"
[ "$RC" -eq 0 ] || die "envsubst: expected exit 0, got $RC" "$OUT"
grep -q "envsubst-ok-$$" <<<"$OUT" || die "envsubst: \${MYVAR} not substituted in the rendered output" "$OUT"

echo "==> enable_helm inflates a LOCAL chart (helm is in the image, offline)"
mkdir -p "$W/eh/charts/mychart/templates"
printf 'apiVersion: v2\nname: mychart\nversion: 0.1.0\n' >"$W/eh/charts/mychart/Chart.yaml"
printf '{}\n' >"$W/eh/charts/mychart/values.yaml"
cat >"$W/eh/charts/mychart/templates/cm.yaml" <<'YAML'
apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Release.Name }}-cm
data:
  key: helm-rendered-ok
YAML
cat >"$W/eh/kustomization.yaml" <<'YAML'
helmGlobals:
  chartHome: charts
helmCharts:
  - name: mychart
    releaseName: smoke
YAML
run . -e PLUGIN_PATH="eh" -e PLUGIN_ACTION="build" -e PLUGIN_ENABLE_HELM="true"
[ "$RC" -eq 0 ] || die "enable_helm: expected exit 0, got $RC — is helm installed?" "$OUT"
grep -q 'name: smoke-cm' <<<"$OUT" || die "enable_helm: the local chart was not inflated (helm missing/not invoked)" "$OUT"
grep -q 'key: helm-rendered-ok' <<<"$OUT" || die "enable_helm: chart template not rendered" "$OUT"

SMOKE_OK=1
echo "PASS: kustomize smoke"
