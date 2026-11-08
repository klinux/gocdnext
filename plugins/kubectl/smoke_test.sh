#!/usr/bin/env bash
# Real smoke for the kubectl plugin's IMAGE. Runs the PINNED binary end to end,
# fully OFFLINE (--network none) — no cluster needed. Proves the bump
# (kubectl v1.31.1 -> v1.37.2, alpine 3.24.4, checksum-verified download):
#   - the binary is v1.37.2;
#   - a missing command fails fast (exit 2);
#   - `kubectl kustomize` renders a kustomization (kubectl + the bundled
#     kustomize work);
#   - a client-side `create --dry-run=client` emits an object, while a manifest
#     missing `kind` fails client-side;
#   - the entrypoint's KUBECONFIG handling decodes a base64 config AND reads a
#     literal one (config current-context round-trips offline).
#
# Convention-named so plugins.yml runs it as a PRE-PUBLISH gate. Needs docker.
# Built --no-cache to match the no_cache publish build. Trap cleanup, fail-closed.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMG="gocdnext-plugin-kubectl:smoke-$$"
W="$(mktemp -d)"
SMOKE_OK=0
cleanup() {
  docker image rm -f "$IMG" >/dev/null 2>&1 || true
  rm -rf "$W" 2>/dev/null || true
  leftover=0
  [ -d "$W" ] && { echo "WARN: cleanup could not remove ${W}" >&2; leftover=1; }
  docker image inspect "$IMG" >/dev/null 2>&1 && { echo "WARN: smoke image ${IMG} not removed" >&2; leftover=1; }
  [ "$leftover" = 1 ] && [ "$SMOKE_OK" = 1 ] && { echo "FAIL: smoke passed but left state behind" >&2; exit 1; }
  return 0
}
trap cleanup EXIT
die() { echo "FAIL: $1"; [ -n "${2:-}" ] && { echo "--- output ---"; echo "$2" | tail -40; }; exit 1; }
run() { # run <env...> : sets RC+OUT, offline, workspace-mounted
  set +e
  OUT="$(docker run --rm --network none -v "$W:/workspace" -w /workspace "$@" "$IMG" 2>&1)"; RC=$?
  set -e
}

echo "==> build plugin image (--no-cache, same Dockerfile/context as the official build)"
docker build --no-cache -t "$IMG" "$HERE" >/dev/null

echo "==> kubectl is v1.37.2"
OUT="$(docker run --rm --network none --entrypoint kubectl "$IMG" version --client 2>&1)" || die "kubectl version failed" "$OUT"
grep -q 'v1.37.2' <<<"$OUT" || die "kubectl is not v1.37.2 (got: ${OUT})" "$OUT"

echo "==> missing command fails fast (exit 2)"
run
[ "$RC" -eq 2 ] || die "missing-command: expected exit 2, got $RC" "$OUT"
grep -q 'PLUGIN_COMMAND is required' <<<"$OUT" || die "missing-command: wrong message" "$OUT"

echo "==> kustomize renders a kustomization (kubectl + bundled kustomize)"
mkdir -p "$W/kust"
printf 'apiVersion: v1\nkind: ConfigMap\nmetadata: {name: smoke-cm}\ndata: {key: value}\n' >"$W/kust/configmap.yaml"
printf 'resources: [configmap.yaml]\nnamePrefix: smoke-\n' >"$W/kust/kustomization.yaml"
run -e PLUGIN_COMMAND="kustomize kust"
[ "$RC" -eq 0 ] || die "kustomize: expected exit 0, got $RC" "$OUT"
grep -q 'name: smoke-smoke-cm' <<<"$OUT" || die "kustomize: namePrefix not applied — kustomize didn't run" "$OUT"

echo "==> create --dry-run=client emits an object (client-side)"
run -e PLUGIN_COMMAND="create configmap foo --from-literal=k=v --dry-run=client -o yaml"
[ "$RC" -eq 0 ] || die "dry-run create: expected exit 0, got $RC" "$OUT"
grep -q 'kind: ConfigMap' <<<"$OUT" || die "dry-run create: no object emitted" "$OUT"

echo "==> a manifest missing 'kind' fails client-side (non-zero)"
printf 'apiVersion: v1\ndata: {k: v}\n' >"$W/nokind.yaml"
run -e PLUGIN_COMMAND="create --dry-run=client --validate=false -f nokind.yaml"
[ "$RC" -ne 0 ] || die "nokind: expected a non-zero exit, got 0" "$OUT"
grep -qi "'Kind' is missing" <<<"$OUT" || die "nokind: unexpected error" "$OUT"

echo "==> KUBECONFIG decodes base64 AND reads literal (config current-context)"
cat >"$W/kc.yaml" <<'YAML'
apiVersion: v1
kind: Config
clusters: [{cluster: {server: https://example.invalid:6443}, name: smoke}]
contexts: [{context: {cluster: smoke, user: smoke}, name: smoke-ctx}]
current-context: smoke-ctx
users: [{name: smoke, user: {token: fake}}]
YAML
B64="$(base64 -w0 "$W/kc.yaml" 2>/dev/null || base64 "$W/kc.yaml" | tr -d '\n')"
run -e PLUGIN_KUBECONFIG="$B64" -e PLUGIN_COMMAND="config current-context"
[ "$RC" -eq 0 ] && grep -q 'smoke-ctx' <<<"$OUT" || die "kubeconfig(base64): expected 'smoke-ctx', got RC=$RC" "$OUT"
# Regression guard: a VALID kubeconfig whose apiVersion/kind sit past any fixed
# byte-window (here behind a long leading comment) must still be detected — the
# detector validates the whole decoded file via `kubectl config view`, not a
# head -c heuristic.
{ printf '# '; head -c 300 /dev/zero | tr '\0' x; printf '\n'; cat "$W/kc.yaml"; } >"$W/kc-comment.yaml"
B64C="$(base64 -w0 "$W/kc-comment.yaml" 2>/dev/null || base64 "$W/kc-comment.yaml" | tr -d '\n')"
run -e PLUGIN_KUBECONFIG="$B64C" -e PLUGIN_COMMAND="config current-context"
[ "$RC" -eq 0 ] && grep -q 'smoke-ctx' <<<"$OUT" || die "kubeconfig(base64+comment): valid config past the byte-window was rejected, RC=$RC" "$OUT"
LIT="$(cat "$W/kc.yaml")"
run -e PLUGIN_KUBECONFIG="$LIT" -e PLUGIN_COMMAND="config current-context"
[ "$RC" -eq 0 ] && grep -q 'smoke-ctx' <<<"$OUT" || die "kubeconfig(literal): expected 'smoke-ctx', got RC=$RC" "$OUT"
# the third accepted shape: a path relative to the workspace.
run -e PLUGIN_KUBECONFIG="kc.yaml" -e PLUGIN_COMMAND="config current-context"
[ "$RC" -eq 0 ] && grep -q 'smoke-ctx' <<<"$OUT" || die "kubeconfig(file path): expected 'smoke-ctx', got RC=$RC" "$OUT"

SMOKE_OK=1
echo "PASS: kubectl smoke"
