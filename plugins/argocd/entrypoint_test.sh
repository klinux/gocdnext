#!/usr/bin/env bash
# Mock-PATH unit test for the argocd plugin entrypoint. No bats — a
# plain bash harness that stubs `argocd` and asserts the argv the
# entrypoint builds. The load-bearing case: PLUGIN_PLUGIN_ENV must
# reach argocd as ONE argument, because a config-management-plugin
# value like "HELM_ARGS=--set image.tag=X -f values.yaml" has spaces
# that word-splitting PLUGIN_COMMAND would shred.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# Stub argocd: record each received arg on its own line so the test
# can assert exact argument boundaries.
cat >"$TMP/argocd" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$@" > "$ARGOCD_ARGS_FILE"
EOF
chmod +x "$TMP/argocd"

run() { ARGOCD_ARGS_FILE="$TMP/args" PATH="$TMP:$PATH" bash "$HERE/entrypoint.sh"; }
fail() { echo "FAIL: $1"; [ -f "$TMP/args" ] && { echo "--- args ---"; cat "$TMP/args"; }; exit 1; }

# ── 1. plugin_env reaches argocd as a single argument (the fix) ──
PLUGIN_SERVER="https://argo.test" PLUGIN_AUTH_TOKEN="tok" \
  PLUGIN_COMMAND="app set my-app" \
  PLUGIN_PLUGIN_ENV="HELM_ARGS=--set global.image.tag=1.2.3 -f ../../platform/configs/values-stage.yaml" \
  PLUGIN_GRPC_WEB="true" PLUGIN_INSECURE="true" run
grep -qx -- "--plugin-env" "$TMP/args" || fail "missing --plugin-env flag"
grep -qx -- "HELM_ARGS=--set global.image.tag=1.2.3 -f ../../platform/configs/values-stage.yaml" "$TMP/args" \
  || fail "plugin_env was word-split (not one arg)"
grep -qx -- "--grpc-web" "$TMP/args" || fail "grpc-web flag dropped"
grep -qx -- "--insecure" "$TMP/args" || fail "insecure flag dropped"

# ── 2. backward compat: no plugin_env → no --plugin-env appended ──
unset PLUGIN_PLUGIN_ENV PLUGIN_GRPC_WEB PLUGIN_INSECURE
PLUGIN_SERVER="https://argo.test" PLUGIN_AUTH_TOKEN="tok" \
  PLUGIN_COMMAND="app sync my-app" run
grep -qx -- "--plugin-env" "$TMP/args" && fail "spurious --plugin-env without the input"
grep -qx -- "sync" "$TMP/args" || fail "command not passed through"

# ── 3. malformed plugin_env is rejected (no NAME=value / newline) ──
rm -f "$TMP/args"
if PLUGIN_SERVER="https://argo.test" PLUGIN_AUTH_TOKEN="tok" \
     PLUGIN_COMMAND="app set my-app" PLUGIN_PLUGIN_ENV="not-an-assignment" run 2>/dev/null; then
    fail "plugin_env without '=' was accepted"
fi
# Name must be an env-ident: the old case-glob let "HELM-ARGS=x" pass.
if PLUGIN_SERVER="https://argo.test" PLUGIN_AUTH_TOKEN="tok" \
     PLUGIN_COMMAND="app set my-app" PLUGIN_PLUGIN_ENV="HELM-ARGS=x" run 2>/dev/null; then
    fail "plugin_env with a non-ident NAME (HELM-ARGS) was accepted"
fi
if PLUGIN_SERVER="https://argo.test" PLUGIN_AUTH_TOKEN="tok" \
     PLUGIN_COMMAND="app set my-app" PLUGIN_PLUGIN_ENV=$'HELM_ARGS=x\nEVIL=y' run 2>/dev/null; then
    fail "plugin_env with a newline was accepted"
fi

echo "PASS: argocd entrypoint"
