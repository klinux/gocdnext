#!/usr/bin/env bash
# Real smoke for the helm plugin's IMAGE. Runs the PINNED binary end to end, fully
# OFFLINE (--network none) — `helm template` renders a local chart without a
# cluster. Proves the bump (helm v3.16.2 -> v3.22.1, alpine 3.24.4,
# checksum-verified download):
#   - the binary is v3.22.1;
#   - a missing command fails fast (exit 2);
#   - `helm template` renders a local chart (the Release name reaches the output).
# Kubeconfig materialisation (0600, /tmp-anchored, content) is covered offline by
# entrypoint_test.sh.
#
# Convention-named so plugins.yml runs it as a PRE-PUBLISH gate. Needs docker.
# Built --no-cache to match the no_cache publish build. Trap cleanup, fail-closed.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMG="gocdnext-plugin-helm:smoke-$$"
W="$(mktemp -d)"
SMOKE_OK=0
cleanup() {
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
run() { set +e; OUT="$(docker run --rm --network none -v "$W:/workspace" -w /workspace "$@" "$IMG" 2>&1)"; RC=$?; set -e; }

echo "==> build plugin image (--no-cache, same Dockerfile/context as the official build)"
docker build --no-cache -t "$IMG" "$HERE" >/dev/null

echo "==> helm is v3.22.1"
OUT="$(docker run --rm --network none --entrypoint helm "$IMG" version --short 2>&1)" || die "helm version failed" "$OUT"
grep -q 'v3.22.1' <<<"$OUT" || die "helm is not v3.22.1 (got: ${OUT})" "$OUT"

echo "==> missing command fails fast (exit 2)"
run
[ "$RC" -eq 2 ] || die "missing-command: expected exit 2, got $RC" "$OUT"
grep -q 'PLUGIN_COMMAND is required' <<<"$OUT" || die "missing-command: wrong message" "$OUT"

echo "==> helm template renders a local chart (offline)"
mkdir -p "$W/chart/templates"
printf 'apiVersion: v2\nname: smokechart\nversion: 0.1.0\n' >"$W/chart/Chart.yaml"
printf '{}\n' >"$W/chart/values.yaml"
cat >"$W/chart/templates/cm.yaml" <<'YAML'
apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Release.Name }}-cm
data:
  key: helm-tmpl-ok
YAML
run -e PLUGIN_COMMAND="template smoke ./chart"
[ "$RC" -eq 0 ] || die "template: expected exit 0, got $RC" "$OUT"
grep -q 'name: smoke-cm' <<<"$OUT" || die "template: Release name not rendered — helm didn't template the chart" "$OUT"
grep -q 'key: helm-tmpl-ok' <<<"$OUT" || die "template: chart template not rendered" "$OUT"

SMOKE_OK=1
echo "PASS: helm smoke"
