#!/usr/bin/env bash
# Mock-PATH unit test for the gravitee plugin entrypoint. No bats — a
# plain bash harness that stubs `gio` (and, for fetch cases, `curl`)
# while using REAL yq/jq/envsubst, so the merge + env-substitution are
# asserted for real and only the Management-API calls are faked.
# Load-bearing cases: create-vs-update by name, plans dropped on update,
# the allowlist-only env substitution (no secret leak), https-only URLs,
# and the bearer token never reaching gio's argv.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

for t in yq jq envsubst; do
    command -v "$t" >/dev/null || { echo "SKIP: $t not installed"; exit 0; }
done

# Stub gio: append each call's argv to GIO_CALLS, and answer the
# `apis list` id-lookup with canned JSON the test controls.
cat >"$TMP/gio" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$GIO_CALLS"
case " $* " in
    *" list "*) echo "${GIO_FAKE_LIST_JSON:-[]}" ;;
esac
exit 0
EOF
chmod +x "$TMP/gio"

fail() {
    echo "FAIL: $1"
    [ -f "$TMP/calls" ] && { echo "--- gio calls ---"; cat "$TMP/calls"; }
    [ -n "${FX:-}" ] && [ -f "$FX/Graviteeio.yml" ] && { echo "--- Graviteeio.yml ---"; cat "$FX/Graviteeio.yml"; }
    exit 1
}

setup_fx() {
    FX="$(mktemp -d "$TMP/fx.XXXX")"
    cat >"$FX/api.yml" <<'YAML'
name: ${API_NAME}
context_path: /orders
plans:
  - name: keyless
YAML
    cat >"$FX/defaults.yml" <<'YAML'
version: "1"
proxy:
  groups:
    - name: default
plans:
  - name: base-plan
YAML
    echo '# template placeholder' >"$FX/tmpl.j2"
    : >"$TMP/calls"
}

run() {
    GIO_CALLS="$TMP/calls" \
    GOCDNEXT_GRAVITEE_VALIDATOR="$HERE/gravitee_validate.py" \
    PATH="$TMP:$PATH" bash "$HERE/entrypoint.sh"
}

# A path-based fixture exercising the method/auth gates:
#   /open    rule with NO methods   → method check flags it; mock = safe
#            for the auth check (terminating, never proxies).
#   /secured explicit methods + oauth2 → clean.
#   /leaky   explicit GET + transform-headers only → auth check flags GET.
setup_paths_fx() {
    FX="$(mktemp -d "$TMP/fx.XXXX")"
    cat >"$FX/api.yml" <<'YAML'
name: ${API_NAME}
context_path: /orders
paths:
  /open:
    - mock:
        status: "404"
  /secured:
    - methods: [GET, POST]
      oauth2:
        oauthResource: KC
  /leaky:
    - methods: [GET]
      transform-headers: {}
YAML
    echo '# template placeholder' >"$FX/tmpl.j2"
    : >"$TMP/calls"
}

# ── 1. create path: no existing API → create --with-start, lint runs,
#       defaults merged + ${API_NAME} substituted, token never on argv ──
setup_fx
GIO_FAKE_LIST_JSON='[]' \
PLUGIN_API_NAME="orders-api" PLUGIN_URL="https://gv.test/mgmt" PLUGIN_TOKEN="s3cr3t-tok" \
PLUGIN_PATH="$FX" PLUGIN_DEFAULTS="$FX/defaults.yml" PLUGIN_TEMPLATE="$FX/tmpl.j2" \
  run >"$TMP/out" 2>&1 || fail "create run errored: $(cat "$TMP/out")"
grep -q 'definition lint' "$TMP/calls"                || fail "lint not called"
grep -q 'definition create --with-start' "$TMP/calls" || fail "create --with-start not called"
grep -q 's3cr3t-tok' "$TMP/calls"                     && fail "token leaked into gio argv"
grep -q 's3cr3t-tok' "$TMP/out"                       && fail "token leaked into echoed output"
grep -q 'name: orders-api' "$FX/Graviteeio.yml"       || fail "API_NAME not substituted"
grep -q 'version: "1"' "$FX/Graviteeio.yml"           || fail "defaults not merged (version missing)"
grep -q 'base-plan' "$FX/Graviteeio.yml" && grep -q 'keyless' "$FX/Graviteeio.yml" \
                                                      || fail "merge mode did not concat plans"
[ -f "$FX/templates/api_config.yml.j2" ]              || fail "template not placed"
[ -d "$FX/settings" ]                                 || fail "settings dir not created"

# ── 2. update path: existing id → apply --api <id> --with-deploy, plans
#       stripped from the payload BY DEFAULT (manage_plans_on_update=false
#       → the import never touches existing plans) ──
setup_fx
GIO_FAKE_LIST_JSON='["api-123"]' \
PLUGIN_API_NAME="orders-api" PLUGIN_URL="https://gv.test/mgmt" PLUGIN_TOKEN="tok" \
PLUGIN_PATH="$FX" PLUGIN_DEFAULTS="$FX/defaults.yml" PLUGIN_TEMPLATE="$FX/tmpl.j2" \
  run >"$TMP/out" 2>&1 || fail "update run errored: $(cat "$TMP/out")"
grep -q 'definition apply --api api-123 --with-deploy' "$TMP/calls" \
                                                      || fail "apply --api --with-deploy not called"
grep -q 'plans' "$FX/Graviteeio.yml"                  && fail "plans not dropped on update (default must be safe)"

# ── 2b. manage_plans_on_update=true KEEPS plans and warns about the
#        subscription risk (opt-in danger) ──
setup_fx
out="$(GIO_FAKE_LIST_JSON='["api-123"]' PLUGIN_MANAGE_PLANS_ON_UPDATE="true" \
  PLUGIN_API_NAME="orders-api" PLUGIN_URL="https://gv.test" PLUGIN_TOKEN="t" \
  PLUGIN_PATH="$FX" PLUGIN_DEFAULTS="$FX/defaults.yml" PLUGIN_TEMPLATE="$FX/tmpl.j2" \
  run 2>&1)" || fail "manage-plans run errored: $out"
grep -q 'plans' "$FX/Graviteeio.yml" || fail "plans dropped despite manage_plans_on_update=true"
echo "$out" | grep -qi 'WARNING.*subscription' || fail "no subscription-risk warning when managing plans"

# ── 3. allowlist substitution: API_NAME + opted-in var substitute, a
#       NON-allowlisted var (a job secret) is left literal, never leaked ──
setup_fx
cat >"$FX/api.yml" <<'YAML'
name: ${API_NAME}
group: ${GROUP}
secret_ref: ${MY_SECRET}
token_ref: ${GIO_APIM_TOKEN}
YAML
GIO_FAKE_LIST_JSON='[]' GROUP="prod-grp" MY_SECRET="shhh-secret" PLUGIN_ENVSUBST_VARS="GROUP" \
PLUGIN_API_NAME="orders-api" PLUGIN_URL="https://gv.test" PLUGIN_TOKEN="leak-me-tok" \
PLUGIN_PATH="$FX" PLUGIN_DEFAULTS="$FX/defaults.yml" PLUGIN_TEMPLATE="$FX/tmpl.j2" \
  run >/dev/null 2>&1 || fail "allowlist run errored"
grep -q 'name: orders-api' "$FX/Graviteeio.yml"       || fail "API_NAME (always allowed) not substituted"
grep -q 'group: prod-grp' "$FX/Graviteeio.yml"        || fail "allowlisted GROUP not substituted"
grep -q 'shhh-secret' "$FX/Graviteeio.yml"            && fail "non-allowlisted secret was substituted (leak)"
grep -q 'leak-me-tok' "$FX/Graviteeio.yml"            && fail "token leaked via \${GIO_APIM_TOKEN}"
grep -qF '${MY_SECRET}' "$FX/Graviteeio.yml"          || fail "non-allowlisted var should stay literal"

# ── 3b. envsubst_vars rejects a credential var name and a bad ident ──
setup_fx
if PLUGIN_ENVSUBST_VARS="GIO_APIM_TOKEN" PLUGIN_API_NAME="x" PLUGIN_URL="https://gv.test" \
     PLUGIN_TOKEN="t" PLUGIN_PATH="$FX" PLUGIN_TEMPLATE="$FX/tmpl.j2" run >/dev/null 2>&1; then
    fail "envsubst_vars accepted a credential var"
fi
if PLUGIN_ENVSUBST_VARS="bad-name" PLUGIN_API_NAME="x" PLUGIN_URL="https://gv.test" \
     PLUGIN_TOKEN="t" PLUGIN_PATH="$FX" PLUGIN_TEMPLATE="$FX/tmpl.j2" run >/dev/null 2>&1; then
    fail "envsubst_vars accepted a non-ident name"
fi
# generic credential-looking name (not a plugin-owned one) is refused
if PLUGIN_ENVSUBST_VARS="DB_PASSWORD" PLUGIN_API_NAME="x" PLUGIN_URL="https://gv.test" \
     PLUGIN_TOKEN="t" PLUGIN_PATH="$FX" PLUGIN_TEMPLATE="$FX/tmpl.j2" run >/dev/null 2>&1; then
    fail "envsubst_vars accepted a credential-looking name (DB_PASSWORD)"
fi

# ── 4. overwrite mode replaces arrays instead of concatenating ──
setup_fx
GIO_FAKE_LIST_JSON='[]' PLUGIN_MODE="overwrite" \
PLUGIN_API_NAME="orders-api" PLUGIN_URL="https://gv.test" PLUGIN_TOKEN="t" \
PLUGIN_PATH="$FX" PLUGIN_DEFAULTS="$FX/defaults.yml" PLUGIN_TEMPLATE="$FX/tmpl.j2" \
  run >/dev/null 2>&1 || fail "overwrite run errored"
grep -q 'keyless' "$FX/Graviteeio.yml"   || fail "overwrite lost api plans"
grep -q 'base-plan' "$FX/Graviteeio.yml" && fail "overwrite did not replace defaults plans"

# ── 5. api_name with a quote is rejected (JMESPath / render injection) ──
setup_fx
if PLUGIN_API_NAME="ev'il" PLUGIN_URL="https://gv.test" PLUGIN_TOKEN="t" \
     PLUGIN_PATH="$FX" PLUGIN_TEMPLATE="$FX/tmpl.j2" run >/dev/null 2>&1; then
    fail "api_name with a quote was accepted"
fi

# ── 6. required inputs enforced ──
setup_fx
if PLUGIN_URL="https://gv.test" PLUGIN_TOKEN="t" PLUGIN_PATH="$FX" run >/dev/null 2>&1; then
    fail "missing api_name was accepted"
fi
if PLUGIN_API_NAME="x" PLUGIN_TOKEN="t" PLUGIN_PATH="$FX" run >/dev/null 2>&1; then
    fail "missing url was accepted"
fi
if PLUGIN_API_NAME="x" PLUGIN_URL="https://gv.test" PLUGIN_PATH="$FX" run >/dev/null 2>&1; then
    fail "missing token was accepted"
fi

# ── 7. http:// is refused for url and for fetched defaults/template ──
setup_fx
if PLUGIN_API_NAME="x" PLUGIN_URL="http://gv.test" PLUGIN_TOKEN="t" \
     PLUGIN_PATH="$FX" PLUGIN_TEMPLATE="$FX/tmpl.j2" run >/dev/null 2>&1; then
    fail "http:// url was accepted"
fi
if PLUGIN_API_NAME="x" PLUGIN_URL="https://user@gv.test" PLUGIN_TOKEN="t" \
     PLUGIN_PATH="$FX" PLUGIN_TEMPLATE="$FX/tmpl.j2" run >/dev/null 2>&1; then
    fail "url with userinfo was accepted"
fi
# a malformed https URL with no host (real-parse / charset check) is refused
if PLUGIN_API_NAME="x" PLUGIN_URL="https://?x" PLUGIN_TOKEN="t" \
     PLUGIN_PATH="$FX" PLUGIN_TEMPLATE="$FX/tmpl.j2" run >/dev/null 2>&1; then
    fail "url with no host (https://?x) was accepted"
fi
if GIO_FAKE_LIST_JSON='[]' PLUGIN_API_NAME="x" PLUGIN_URL="https://gv.test" PLUGIN_TOKEN="t" \
     PLUGIN_PATH="$FX" PLUGIN_DEFAULTS="http://cfg.test/d.yml" PLUGIN_TEMPLATE="$FX/tmpl.j2" \
     run >/dev/null 2>&1; then
    fail "http:// defaults URL was accepted"
fi

# ── 8. multiple APIs with the same name → refuse (don't update the wrong one) ──
setup_fx
if GIO_FAKE_LIST_JSON='["api-1","api-2"]' \
     PLUGIN_API_NAME="orders-api" PLUGIN_URL="https://gv.test" PLUGIN_TOKEN="t" \
     PLUGIN_PATH="$FX" PLUGIN_DEFAULTS="$FX/defaults.yml" PLUGIN_TEMPLATE="$FX/tmpl.j2" \
     run >/dev/null 2>&1; then
    fail "ambiguous name (2 matches) was not refused"
fi

# ── 9. boolean inputs reject typos (no silent false) ──
setup_fx
if GIO_FAKE_LIST_JSON='[]' PLUGIN_DEPLOY="yes" \
     PLUGIN_API_NAME="x" PLUGIN_URL="https://gv.test" PLUGIN_TOKEN="t" \
     PLUGIN_PATH="$FX" PLUGIN_TEMPLATE="$FX/tmpl.j2" run >/dev/null 2>&1; then
    fail "deploy=yes (typo) was accepted as a bool"
fi

# ── 10. config_token rides a curl --config file, NEVER argv; no -L ──
setup_fx
cat >"$TMP/curl" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$@" >> "$CURL_ARGS"
dest=""; cfg=""; prev=""
for a in "$@"; do
    [ "$prev" = "-o" ] && dest="$a"
    [ "$prev" = "--config" ] && cfg="$a"
    prev="$a"
done
if [ -n "$cfg" ]; then
    cat "$cfg" >> "$CURL_CFG_DUMP"
    dest="$(sed -n 's/^output = "\(.*\)"/\1/p' "$cfg")"
fi
[ -n "$dest" ] && printf 'version: "9"\n' > "$dest"
exit 0
EOF
chmod +x "$TMP/curl"
CURL_ARGS="$TMP/curlargs" CURL_CFG_DUMP="$TMP/cfgdump" GIO_FAKE_LIST_JSON='[]' \
PLUGIN_API_NAME="orders-api" PLUGIN_URL="https://gv.test" PLUGIN_TOKEN="t" \
PLUGIN_PATH="$FX" PLUGIN_DEFAULTS="https://cfg.test/defaults.yml" PLUGIN_CONFIG_TOKEN="cfg-tok" \
PLUGIN_TEMPLATE="$FX/tmpl.j2" \
  run >/dev/null 2>&1 || fail "url-fetch run errored"
grep -q -- '--config' "$TMP/curlargs"                  || fail "curl did not use a --config file"
grep -q 'cfg-tok' "$TMP/curlargs"                      && fail "config_token leaked into curl argv"
grep -q -- '-L' "$TMP/curlargs"                        && fail "curl followed redirects (-L) with a token"
grep -q 'Authorization: token cfg-tok' "$TMP/cfgdump"  || fail "config_token not sent via the config file"
grep -q 'version: "9"' "$FX/Graviteeio.yml"            || fail "fetched defaults not merged"
rm -f "$TMP/curl"

# ── 11. method_policy=block fails on a rule without explicit methods;
#        warn logs /open but still applies ──
setup_paths_fx
if GIO_FAKE_LIST_JSON='[]' PLUGIN_METHOD_POLICY="block" \
     PLUGIN_API_NAME="orders-api" PLUGIN_URL="https://gv.test" PLUGIN_TOKEN="t" \
     PLUGIN_PATH="$FX" PLUGIN_TEMPLATE="$FX/tmpl.j2" run >/dev/null 2>&1; then
    fail "method_policy=block did not fail on a methodless rule"
fi
setup_paths_fx
out="$(GIO_FAKE_LIST_JSON='[]' PLUGIN_METHOD_POLICY="warn" \
     PLUGIN_API_NAME="orders-api" PLUGIN_URL="https://gv.test" PLUGIN_TOKEN="t" \
     PLUGIN_PATH="$FX" PLUGIN_TEMPLATE="$FX/tmpl.j2" run 2>&1)" || fail "method warn should not fail: $out"
echo "$out" | grep -qi 'WARNING.*/open' || fail "method warn did not flag /open"
grep -q 'definition create' "$TMP/calls" || fail "method warn should still apply"

# ── 12. auth_policy=block fails on an unauthenticated method; mock-only
#        path is treated as safe (no auth finding for /open) ──
setup_paths_fx
if GIO_FAKE_LIST_JSON='[]' PLUGIN_METHOD_POLICY="off" PLUGIN_AUTH_POLICY="block" \
     PLUGIN_API_NAME="orders-api" PLUGIN_URL="https://gv.test" PLUGIN_TOKEN="t" \
     PLUGIN_PATH="$FX" PLUGIN_TEMPLATE="$FX/tmpl.j2" run >/dev/null 2>&1; then
    fail "auth_policy=block did not fail on an unauthenticated method"
fi
setup_paths_fx
out="$(GIO_FAKE_LIST_JSON='[]' PLUGIN_METHOD_POLICY="off" PLUGIN_AUTH_POLICY="warn" \
     PLUGIN_API_NAME="orders-api" PLUGIN_URL="https://gv.test" PLUGIN_TOKEN="t" \
     PLUGIN_PATH="$FX" PLUGIN_TEMPLATE="$FX/tmpl.j2" run 2>&1)" || fail "auth warn should not fail"
echo "$out" | grep -qi 'WARNING.*/leaky.*GET' || fail "auth warn did not flag /leaky GET"
echo "$out" | grep -qi 'WARNING.*/open' && fail "mock-only /open should not be auth-flagged"

# ── 13. invalid policy level is rejected ──
setup_fx
if PLUGIN_METHOD_POLICY="loud" PLUGIN_API_NAME="x" PLUGIN_URL="https://gv.test" \
     PLUGIN_TOKEN="t" PLUGIN_PATH="$FX" PLUGIN_TEMPLATE="$FX/tmpl.j2" run >/dev/null 2>&1; then
    fail "invalid method_policy level was accepted"
fi

# ── 14. a DISABLED auth rule does not count as coverage: GET has a
#        disabled oauth2 + an enabled transform-headers → auth=block must
#        still flag GET ──
setup_paths_fx
cat >"$FX/api.yml" <<'YAML'
name: ${API_NAME}
paths:
  /g:
    - methods: [GET]
      enabled: false
      oauth2:
        oauthResource: KC
    - methods: [GET]
      enabled: true
      transform-headers: {}
YAML
: >"$TMP/calls"
if GIO_FAKE_LIST_JSON='[]' PLUGIN_METHOD_POLICY="off" PLUGIN_AUTH_POLICY="block" \
     PLUGIN_API_NAME="orders-api" PLUGIN_URL="https://gv.test" PLUGIN_TOKEN="t" \
     PLUGIN_PATH="$FX" PLUGIN_TEMPLATE="$FX/tmpl.j2" run >/dev/null 2>&1; then
    fail "a disabled oauth2 rule was wrongly counted as auth coverage"
fi

# ── 15. an empty rule list (no rule matches → all methods open) is
#        flagged by the methods check ──
setup_paths_fx
cat >"$FX/api.yml" <<'YAML'
name: ${API_NAME}
paths:
  /empty: []
YAML
: >"$TMP/calls"
if GIO_FAKE_LIST_JSON='[]' PLUGIN_METHOD_POLICY="block" \
     PLUGIN_API_NAME="orders-api" PLUGIN_URL="https://gv.test" PLUGIN_TOKEN="t" \
     PLUGIN_PATH="$FX" PLUGIN_TEMPLATE="$FX/tmpl.j2" run >/dev/null 2>&1; then
    fail "empty rule list (/empty: []) was not flagged as open"
fi

echo "PASS: gravitee entrypoint"
