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

# ── 4. the resolved invocation is echoed (visibility) and the auth
#       token never leaks into that line — `app set` is silent, so the
#       echo is the only signal a deploy step produces. ──
rm -f "$TMP/args"
out="$(PLUGIN_SERVER="https://argo.test" PLUGIN_AUTH_TOKEN="s3cr3t-tok" \
  PLUGIN_COMMAND="app set my-app" PLUGIN_GRPC_WEB="true" run)"
echo "$out" | grep -qE '^==> argocd .*app set my-app' || fail "resolved command not echoed for visibility"
echo "$out" | grep -q "s3cr3t-tok" && fail "auth token leaked into the echoed command"

# ── 5. ca_cert is written to a file and passed via --server-crt (verify path) ──
rm -f "$TMP/args"
PEM=$'-----BEGIN CERTIFICATE-----\nMIIBfakeCApayloadForTest\n-----END CERTIFICATE-----'
PLUGIN_SERVER="https://argo.test" PLUGIN_AUTH_TOKEN="tok" \
  PLUGIN_COMMAND="app sync my-app" PLUGIN_CA_CERT="$PEM" run
grep -qx -- "--server-crt" "$TMP/args" || fail "ca_cert did not add --server-crt"
# The stub records each arg on its own line, so the CA file path is the line
# right after --server-crt. `exec argocd` replaces the shell, so the entrypoint's
# EXIT trap never fires — the temp file survives for this assertion (and dies with
# the container in prod).
crt_path="$(grep -A1 -x -- '--server-crt' "$TMP/args" | tail -1)"
[ -n "$crt_path" ] && [ -f "$crt_path" ] || fail "--server-crt path does not exist ($crt_path)"
grep -q "BEGIN CERTIFICATE" "$crt_path" || fail "ca_cert PEM not written to the --server-crt file"
grep -qx -- "--insecure" "$TMP/args" && fail "insecure must not be set when only ca_cert is given"
rm -f "$crt_path"

# ── 6. insecure + ca_cert together is rejected (contradiction, fails loud) ──
if PLUGIN_SERVER="https://argo.test" PLUGIN_AUTH_TOKEN="tok" \
     PLUGIN_COMMAND="app sync my-app" PLUGIN_INSECURE="true" PLUGIN_CA_CERT="$PEM" run 2>/dev/null; then
    fail "insecure + ca_cert both set was accepted (should exit non-zero)"
fi

# ── 7. a non-PEM ca_cert is rejected (a garbled secret ≠ a silent net error) ──
if PLUGIN_SERVER="https://argo.test" PLUGIN_AUTH_TOKEN="tok" \
     PLUGIN_COMMAND="app sync my-app" PLUGIN_CA_CERT="not-a-pem" run 2>/dev/null; then
    fail "non-PEM ca_cert was accepted (should exit non-zero)"
fi

echo "PASS: argocd entrypoint"
