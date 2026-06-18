#!/usr/bin/env bash
# gocdnext/gravitee — configure a Gravitee API definition through the
# graviteeio-cli (`gio`). See Dockerfile for the full input contract.
#
# One step that mirrors the legacy GoCD MakefileGravitee:
#   resolve defaults/template (path-or-URL) -> yq merge -> envsubst
#   -> Graviteeio.yml -> gio lint -> look up the API id by name
#   -> create (--with-start) | apply --api <id> (--with-deploy).
#
# The bearer token rides GIO_APIM_TOKEN in the env — NEVER on argv —
# so it can't leak into the process table or the echoed invocation.

set -euo pipefail

die() { echo "gocdnext/gravitee: $*" >&2; exit 2; }

# parse_bool rejects a typo that would otherwise read as a silent false
# (e.g. deploy: yes → no deploy, no warning).
parse_bool() {
    case "$1" in
        true | false) printf '%s' "$1" ;;
        *) die "$2 must be 'true' or 'false' (got '$1')" ;;
    esac
}

# parse_level validates a tri-state security gate (off|warn|block).
parse_level() {
    case "$1" in
        off | warn | block) printf '%s' "$1" ;;
        *) die "$2 must be 'off', 'warn', or 'block' (got '$1')" ;;
    esac
}

# require_https enforces an https:// URL with a host and no userinfo: a
# bearer token must never ride cleartext http, and a config/template URL
# must not smuggle credentials in the authority or be tamperable. Uses a
# real URL parse via python (the image ships it) and falls back to a
# strict pattern check so the entrypoint still validates without python.
require_https() {
    local v="$1" field="$2" rest host
    case "$v" in
        https://*) ;;
        http://*) die "$field must be https:// — refusing cleartext http ('$v')" ;;
        *) die "$field must be an https:// URL (got '$v')" ;;
    esac
    if command -v python3 >/dev/null 2>&1; then
        python3 - "$v" "$field" <<'PY' || exit $?
import sys, urllib.parse
v, field = sys.argv[1], sys.argv[2]
u = urllib.parse.urlparse(v)
if not u.hostname:
    sys.stderr.write(f"gocdnext/gravitee: {field} must include a host ('{v}')\n"); sys.exit(2)
if u.username or u.password:
    sys.stderr.write(f"gocdnext/gravitee: {field} must not contain userinfo ('{v}')\n"); sys.exit(2)
PY
    else
        rest="${v#https://}"
        host="${rest%%/*}"
        case "$host" in
            "") die "$field must include a host" ;;
            *@*) die "$field must not contain userinfo (user@host)" ;;
            *[!A-Za-z0-9.:_[\]-]*) die "$field has an invalid host ('$host')" ;;
        esac
    fi
}

# ── required inputs ──
[ -n "${PLUGIN_API_NAME:-}" ] || die "api_name is required"
[ -n "${PLUGIN_URL:-}" ]      || die "url is required (the Gravitee Management API base URL)"
[ -n "${PLUGIN_TOKEN:-}" ]    || die "token is required — reference a secret: with: { token: \${{ GRAVITEE_TOKEN }} }"
require_https "$PLUGIN_URL" "url"

# api_name is interpolated into the gio JMESPath query (-q "[?name=='…']")
# and the rendered config; reject quotes/newlines so it can't break out
# of the query or smuggle a second line into the file.
case "$PLUGIN_API_NAME" in
    *\'* | *\"* | *$'\n'*) die "api_name must not contain quotes or newlines" ;;
esac

path_dir="${PLUGIN_PATH:-.}"
values_file="${PLUGIN_VALUES_FILE:-api.yml}"
mode="${PLUGIN_MODE:-merge}"
deploy="$(parse_bool "${PLUGIN_DEPLOY:-true}" deploy)"
lint="$(parse_bool "${PLUGIN_LINT:-true}" lint)"
# DANGER knob — see plugin.yaml. Default false = strip plans from the
# update payload so the import never touches existing plans (and their
# subscriptions). true lets Gravitee reconcile plans on update, which
# can alter/close/remove a plan and break its ACTIVE SUBSCRIPTIONS.
manage_plans="$(parse_bool "${PLUGIN_MANAGE_PLANS_ON_UPDATE:-false}" manage_plans_on_update)"
# Security gates for the keyless path-based model (see plugin.yaml).
# method_policy defaults to warn (surface open methods everywhere); the
# heuristic auth_policy check is opt-in (off) to avoid false-positives on
# intentionally-public paths.
method_policy="$(parse_level "${PLUGIN_METHOD_POLICY:-warn}" method_policy)"
auth_policy="$(parse_level "${PLUGIN_AUTH_POLICY:-off}" auth_policy)"

[ -d "$path_dir" ] || die "path '$path_dir' is not a directory"
cd "$path_dir"
[ -f "$values_file" ] || die "values_file '$values_file' not found under path '$path_dir'"

# ── gio auth env (token never on argv) ──
export GIO_APIM_URL="$PLUGIN_URL"
export GIO_APIM_TOKEN="$PLUGIN_TOKEN"
export GIO_APIM_ORG="${PLUGIN_ORG:-DEFAULT}"
export GIO_APIM_ENV="${PLUGIN_ENV:-DEFAULT}"

# TLS: a CA bundle (PEM or base64) pins verification; otherwise honour
# the ssl_verify flag (true/false/path). gocdnext's stance mirrors the
# cluster registry — prefer a pinned CA over skip-verify.
if [ -n "${PLUGIN_CA_BUNDLE:-}" ]; then
    ca=/tmp/gocdnext-gravitee-ca.crt
    if printf '%s' "$PLUGIN_CA_BUNDLE" | base64 -d >"$ca" 2>/dev/null && grep -q 'CERTIFICATE' "$ca"; then
        : # decoded base64 PEM
    else
        printf '%s' "$PLUGIN_CA_BUNDLE" >"$ca" # literal PEM
    fi
    chmod 0600 "$ca"
    export GIO_SSL_VERIFY="$ca"
    export REQUESTS_CA_BUNDLE="$ca"
elif [ -n "${PLUGIN_SSL_VERIFY:-}" ]; then
    export GIO_SSL_VERIFY="$PLUGIN_SSL_VERIFY"
fi

# fetch resolves a "path-or-URL" input into a local file. URLs must be
# https (no cleartext, no userinfo). A config_token rides a 0600 curl
# --config file, NEVER argv, and we don't follow redirects (-L) so the
# Authorization header can't be replayed to another host.
fetch() {
    local src="$1" dest="$2" cfg
    case "$src" in
        http://*) die "refusing cleartext http for '$src' — use https://" ;;
        https://*)
            require_https "$src" "fetch URL"
            if [ -n "${PLUGIN_CONFIG_TOKEN:-}" ]; then
                cfg="$(mktemp)"
                chmod 0600 "$cfg"
                {
                    printf 'url = "%s"\n' "$src"
                    printf 'output = "%s"\n' "$dest"
                    printf 'header = "Authorization: token %s"\n' "$PLUGIN_CONFIG_TOKEN"
                } >"$cfg"
                if ! curl -fsS --config "$cfg"; then
                    rm -f "$cfg"
                    die "fetch failed: $src"
                fi
                rm -f "$cfg"
            else
                curl -fsS -o "$dest" "$src" || die "fetch failed: $src"
            fi
            ;;
        *)
            [ -f "$src" ] || die "file not found: $src"
            cp "$src" "$dest"
            ;;
    esac
}

# gio resolves templates/api_config.yml.j2 + settings/ under the def-path
# (here, the current dir). Both folders must exist or gio errors.
mkdir -p templates settings
if [ -n "${PLUGIN_TEMPLATE:-}" ]; then
    fetch "$PLUGIN_TEMPLATE" templates/api_config.yml.j2
fi
[ -f templates/api_config.yml.j2 ] || \
    die "no template: set 'template' (path or URL) or provide templates/api_config.yml.j2 under path"

# ── merge defaults + values, then env-substitute ──
merged=Graviteeio.tmp
if [ -n "${PLUGIN_DEFAULTS:-}" ]; then
    defaults=/tmp/gocdnext-gravitee-defaults.yml
    fetch "$PLUGIN_DEFAULTS" "$defaults"
    case "$mode" in
        merge)     yq eval-all '. as $item ireduce ({}; . *+ $item)' "$defaults" "$values_file" >"$merged" ;;
        overwrite) yq '. *= load("'"$values_file"'")' "$defaults" >"$merged" ;;
        *) die "mode must be 'merge' or 'overwrite' (got '$mode')" ;;
    esac
else
    cp "$values_file" "$merged"
fi

# Substitute ${VAR} placeholders against an explicit ALLOWLIST. By
# default only ${API_NAME} is substituted; the operator opts extra
# NON-secret vars in via `envsubst_vars` (e.g. GO_PIPELINE_GROUP_NAME).
# A ${VAR} not on the list is left literal, so a job secret living in
# the environment can never be substituted into the payload sent to
# Gravitee. Single pass (envsubst never re-expands its own output).
allow_vars=(API_NAME)
if [ -n "${PLUGIN_ENVSUBST_VARS:-}" ]; then
    IFS=', ' read -r -a _extra <<<"$PLUGIN_ENVSUBST_VARS"
    for v in "${_extra[@]}"; do
        [ -z "$v" ] && continue
        # Belt-and-suspenders over the allowlist: refuse any name that
        # looks like a credential (is, or ends in, TOKEN/SECRET/PASSWORD/
        # PASSWD/KEY/CRED/CREDENTIAL[S]) so an operator can't opt a secret
        # in even by accident. Covers the plugin's own creds too.
        case "${v^^}" in
            TOKEN | SECRET | SECRETS | PASSWORD | PASSWD | KEY | CRED | CREDENTIAL | CREDENTIALS \
                | *_TOKEN | *_SECRET | *_SECRETS | *_PASSWORD | *_PASSWD | *_KEY | *_CRED | *_CREDENTIAL | *_CREDENTIALS)
                die "envsubst_vars must not include a credential-looking var ('$v')" ;;
        esac
        [[ "$v" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] || die "envsubst_vars entry '$v' is not a valid env var name"
        allow_vars+=("$v")
    done
fi
shell_format=""
for v in "${allow_vars[@]}"; do shell_format+="\${${v}} "; done
API_NAME="$PLUGIN_API_NAME" envsubst "$shell_format" <"$merged" >Graviteeio.yml
rm -f "$merged"

# ── security gate: open methods under the keyless default plan ──
# Convert the rendered definition to JSON (the validator is stdlib-only)
# and run the two checks at their configured levels. block → fail here,
# before anything is written to the Management API.
if [ "$method_policy" != "off" ] || [ "$auth_policy" != "off" ]; then
    validator="${GOCDNEXT_GRAVITEE_VALIDATOR:-/usr/local/bin/gravitee-validate}"
    defn_json="$(mktemp)"
    yq -o=json '.' Graviteeio.yml >"$defn_json"
    if ! python3 "$validator" --methods "$method_policy" --auth "$auth_policy" \
        --auth-policies "${PLUGIN_AUTH_POLICIES:-oauth2,jwt,api-key}" "$defn_json"; then
        rm -f "$defn_json"
        die "method/auth policy validation failed (see ERROR lines above)"
    fi
    rm -f "$defn_json"
fi

# ── lint (apply lints internally too; the explicit pass surfaces it
#    early, before any write to the Management API) ──
if [ "$lint" = "true" ]; then
    gio apim apis definition lint
fi

# ── create-or-update by name ──
# gio's `apply` creates when no --api is given and updates when one is;
# we resolve the id by name so an existing API is UPDATED, never
# duplicated. More than one match is ambiguous — refuse rather than
# silently update the wrong API.
ids_json="$(gio apim apis list -q "[?name=='${PLUGIN_API_NAME}'].id" -o json)"
match_count="$(printf '%s' "$ids_json" | jq 'length')"
if [ "$match_count" -gt 1 ]; then
    die "found ${match_count} APIs named '${PLUGIN_API_NAME}' — refusing to guess which to update; disambiguate in Gravitee"
fi
api_id="$(printf '%s' "$ids_json" | jq -r '.[0] // empty')"

deploy_flag=()
if [ -n "$api_id" ] && [ "$api_id" != "null" ]; then
    if [ "$manage_plans" = "true" ]; then
        # Opt-in DANGER: plans stay in the payload, so the import
        # reconciles them on the server. A plan that gets altered,
        # closed, or removed takes its ACTIVE SUBSCRIPTIONS down with it.
        echo "WARNING: manage_plans_on_update=true — the import will reconcile plans on '${PLUGIN_API_NAME}'. A modified/closed/removed plan BREAKS its active subscriptions; make sure the payload's plans match production." >&2
    else
        # Default + safe: drop plans from the update payload so the
        # import leaves existing plans (and subscriptions) untouched.
        yq -i 'del(.plans)' Graviteeio.yml
    fi
    if [ "$deploy" = "true" ]; then
        deploy_flag=(--with-deploy)
    fi
    echo "==> gio apim apis definition apply --api ${api_id} ${deploy_flag[*]}  (API '${PLUGIN_API_NAME}')"
    exec gio apim apis definition apply --api "$api_id" "${deploy_flag[@]}"
else
    if [ "$deploy" = "true" ]; then
        deploy_flag=(--with-start)
    fi
    echo "==> gio apim apis definition create ${deploy_flag[*]}  (new API '${PLUGIN_API_NAME}')"
    exec gio apim apis definition create "${deploy_flag[@]}"
fi
