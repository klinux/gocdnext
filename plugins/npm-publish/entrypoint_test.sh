#!/usr/bin/env bash
# Mock-PATH test for the npm-publish entrypoint. Two auth modes must produce the
# right USER-scoped ~/.npmrc without any secret reaching argv:
#   - token: //host/path/:_authToken=<NPM_TOKEN>
#   - basic: //host/path/:_auth=<base64(user:pass)>  (Nexus/Artifactory/Verdaccio)
# Plus: idempotent skip when name@version already exists.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# Mock npm: log every call; `view` exits per MOCK_VIEW_EXISTS; publish is a no-op.
cat >"$TMP/npm" <<EOF
#!/usr/bin/env bash
echo "npm \$*" >> "$TMP/calls.log"
case "\$1" in
  view) [ "\${MOCK_VIEW_EXISTS:-0}" = "1" ] && exit 0 || exit 1 ;;
  *)    exit 0 ;;
esac
EOF
chmod +x "$TMP/npm"

mkdir -p "$TMP/pkg"
cat >"$TMP/pkg/package.json" <<'EOF'
{ "name": "@platform/widget", "version": "1.2.3" }
EOF

REG="https://nexus.example/repository/npm-hosted/"
fail() { echo "FAIL: $1"; echo "--- calls ---"; cat "$TMP/calls.log" 2>/dev/null; echo "--- npmrc ---"; cat "$TMP/home/.npmrc" 2>/dev/null; exit 1; }

run() { # env-vars... — fresh HOME each run
    rm -rf "$TMP/home"; mkdir -p "$TMP/home"; : > "$TMP/calls.log"
    env "$@" HOME="$TMP/home" PLUGIN_DIR="$TMP/pkg" PLUGIN_REGISTRY="$REG" \
        PATH="$TMP:$PATH" bash "$HERE/entrypoint.sh" >/dev/null 2>&1
}

# --- basic auth (username + password) → _auth base64, always-auth, publish ---
run PLUGIN_USERNAME="deployer" PLUGIN_PASSWORD="s3cr3t"
B64=$(printf '%s:%s' "deployer" "s3cr3t" | base64 | tr -d '\n')
grep -qF "//nexus.example/repository/npm-hosted/:_auth=${B64}" "$TMP/home/.npmrc" || fail "basic: _auth line missing/wrong"
grep -qF "//nexus.example/repository/npm-hosted/:always-auth=true" "$TMP/home/.npmrc" || fail "basic: always-auth missing"
grep -q "npm publish" "$TMP/calls.log" || fail "basic: publish not invoked"
grep -qF "s3cr3t" "$TMP/calls.log" && fail "basic: password leaked onto npm argv"
grep -qF "${B64}" "$TMP/calls.log" && fail "basic: _auth base64 leaked onto npm argv"

# --- token auth (NPM_TOKEN) → _authToken ---
run NPM_TOKEN="tok-abc"
grep -qF "//nexus.example/repository/npm-hosted/:_authToken=tok-abc" "$TMP/home/.npmrc" || fail "token: _authToken line missing/wrong"
grep -q "npm publish" "$TMP/calls.log" || fail "token: publish not invoked"
grep -qF "tok-abc" "$TMP/calls.log" && fail "token: token leaked onto npm argv"

# --- idempotent skip: if-exists=skip AND version already published → no publish ---
run PLUGIN_USERNAME="deployer" PLUGIN_PASSWORD="s3cr3t" PLUGIN_IF_EXISTS="skip" MOCK_VIEW_EXISTS="1"
grep -q "npm publish" "$TMP/calls.log" && fail "skip: published despite existing version"
grep -q "npm view" "$TMP/calls.log" || fail "skip: existence check not run"

# --- no auth + not dry-run → must fail (exit != 0) ---
if run PLUGIN_IF_EXISTS="fail"; then
    fail "no-auth: expected failure without token or username/password"
fi

# --- registry URL with embedded credentials → fail-closed (no leak to log/argv) ---
rm -rf "$TMP/home"; mkdir -p "$TMP/home"; : > "$TMP/calls.log"
if env NPM_TOKEN="tok" HOME="$TMP/home" PLUGIN_DIR="$TMP/pkg" \
       PLUGIN_REGISTRY="https://sneaky:leak@nexus.example/repository/npm-hosted/" \
       PATH="$TMP:$PATH" bash "$HERE/entrypoint.sh" >"$TMP/out.log" 2>&1; then
    fail "embedded-creds registry: expected fail-closed rejection"
fi
grep -qF "leak" "$TMP/calls.log" && fail "embedded-creds: credential reached npm argv"
grep -qF "leak" "$TMP/home/.npmrc" 2>/dev/null && fail "embedded-creds: credential written to .npmrc"

echo "PASS: npm-publish entrypoint"
