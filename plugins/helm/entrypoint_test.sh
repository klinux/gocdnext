#!/usr/bin/env bash
# Mock-PATH unit test for the helm plugin entrypoint. Stubs `helm` (records argv +
# the mode/path/content-hash of the materialised $KUBECONFIG) and asserts:
#   - PLUGIN_COMMAND is required (exit 2) and is word-split into helm's argv;
#   - PLUGIN_NAMESPACE becomes `--namespace <ns>` ahead of the command;
#   - the kubeconfig (tokens/certs) is materialised 0600, at a FRESH /tmp-anchored
#     path (never a job-controlled $TMPDIR, never reused), with the CORRECT bytes,
#     for file / base64 / base64-behind-a-long-comment / literal.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
export MODE_LOG="$TMP/mode.log" HASH_LOG="$TMP/hash.log"

cat >"$TMP/helm" <<EOF
#!/usr/bin/env bash
printf 'helm %s\n' "\$*" >> "$TMP/helm.log"
if [ -n "\${KUBECONFIG:-}" ] && [ -e "\${KUBECONFIG}" ]; then
    stat -c '%a %n' "\${KUBECONFIG}" >> "$MODE_LOG" 2>/dev/null || true
    sha256sum "\${KUBECONFIG}" 2>/dev/null | awk '{print \$1}' >> "$HASH_LOG" || true
fi
exit "\${MOCK_RC:-0}"
EOF
chmod +x "$TMP/helm"

fails=0
pass() { echo "PASS: $1"; }
bad()  { echo "FAIL: $1"; fails=1; }
runep() { : >"$TMP/helm.log"; env "$@" PATH="$TMP:$PATH" bash "$HERE/entrypoint.sh" >/dev/null 2>&1; }

echo "== 1. missing command -> exit 2 =="
runep PLUGIN_COMMAND=""; [ $? -eq 2 ] && pass "missing command exit 2" || bad "missing command"

echo "== 2. command word-split into helm argv =="
runep PLUGIN_COMMAND="upgrade --install api ./chart"
grep -qx "helm upgrade --install api ./chart" "$TMP/helm.log" || bad "command not passed verbatim"
[ "$fails" = 0 ] && pass "command word-split"

echo "== 3. namespace -> --namespace <ns> before the command =="
: >"$TMP/helm.log"
runep PLUGIN_COMMAND="list" PLUGIN_NAMESPACE="apps"
grep -qx "helm --namespace apps list" "$TMP/helm.log" || bad "namespace not prepended"
grep -qx "helm --namespace apps list" "$TMP/helm.log" && pass "namespace prepended"

echo "== 4. kubeconfig materialised 0600, fresh /tmp path, correct content =="
cat >"$TMP/kc.yaml" <<'YAML'
apiVersion: v1
kind: Config
clusters: [{cluster: {server: https://example.invalid:6443}, name: smoke}]
contexts: [{context: {cluster: smoke, user: smoke}, name: smoke-ctx}]
current-context: smoke-ctx
users: [{name: smoke, user: {token: fake}}]
YAML
{ printf '# '; head -c 300 /dev/zero | tr '\0' x; printf '\n'; cat "$TMP/kc.yaml"; } >"$TMP/kc-comment.yaml"
KC_B64="$(base64 -w0 "$TMP/kc.yaml")"; KC_B64C="$(base64 -w0 "$TMP/kc-comment.yaml")"
KC_LIT="$(cat "$TMP/kc.yaml")"; printf '%s' "$KC_LIT" >"$TMP/kc-lit-expected.yaml"
PREV=""
check_kc() { # <desc> <PLUGIN_KUBECONFIG> <expected-content-file>
    : >"$MODE_LOG"; : >"$HASH_LOG"
    ( umask 000; env PLUGIN_KUBECONFIG="$2" PLUGIN_COMMAND="list" TMPDIR="$TMP" \
        PATH="$TMP:$PATH" bash "$HERE/entrypoint.sh" >/dev/null 2>&1 )
    local line mode path; line="$(tail -n1 "$MODE_LOG")"; mode="${line%% *}"; path="${line#* }"
    [ "$mode" = "600" ] || { bad "kubeconfig($1): mode ${mode}, want 600"; return; }
    case "$path" in /tmp/gocdnext-kubeconfig.??????*) : ;; *) rm -f "$path"; bad "kubeconfig($1): not /tmp-anchored ($path)"; return ;; esac
    [ -n "$PREV" ] && [ "$path" = "$PREV" ] && { rm -f "$path"; bad "kubeconfig($1): reused dest $path"; return; }
    local want got; want="$(sha256sum "$3" | awk '{print $1}')"; got="$(tail -n1 "$HASH_LOG")"
    [ "$got" = "$want" ] || { rm -f "$path"; bad "kubeconfig($1): content hash mismatch"; return; }
    PREV="$path"; rm -f "$path"; pass "kubeconfig($1): 0600, fresh, content verified"
}
check_kc "file"           "$TMP/kc.yaml"  "$TMP/kc.yaml"
check_kc "base64"         "$KC_B64"       "$TMP/kc.yaml"
check_kc "base64+comment" "$KC_B64C"      "$TMP/kc-comment.yaml"
check_kc "literal"        "$KC_LIT"       "$TMP/kc-lit-expected.yaml"

if [ "$fails" = 0 ]; then echo "ALL PASS"; else echo "SOME FAILED"; exit 1; fi
