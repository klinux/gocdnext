#!/usr/bin/env bash
# Mock-level test for the helm-push entrypoint. NO docker, NO network: it puts
# fake `helm` and `curl` on PATH that record their argv (and, for curl, the
# contents of any --config file) and asserts the plugin's SECURITY contract:
#
#   - the registry password NEVER reaches argv (visible via /proc/<pid>/cmdline)
#     — not for curl (chartmuseum/nexus) and not for helm (oci login, via stdin);
#   - chartmuseum/nexus hand curl a single-line `user = "u:p"` --config file, so
#     a password with a quote/backslash can't inject a second curl option.
#
# Convention-named so plugins.yml runs it as a pre-publish gate (mock stage).
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ENTRY="${HERE}/entrypoint.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

BIN="${TMP}/bin"; mkdir -p "${BIN}"
export ARGV_LOG="${TMP}/argv.log"
export CONFIG_LOG="${TMP}/config.log"

# A password that MUST survive escaping without leaking or injecting: it carries
# a double quote and a backslash (curl config metacharacters) plus a plain,
# greppable MARKER token.
PW='P@ss"\MARKER'

cat >"${BIN}/helm" <<'EOF'
#!/usr/bin/env bash
printf 'helm %s\n' "$*" >>"${ARGV_LOG}"
if [ "${1:-}" = "registry" ] && [ "${2:-}" = "login" ]; then
    cat >/dev/null   # drain --password-stdin so the pipe doesn't SIGPIPE
fi
if [ "${1:-}" = "package" ]; then
    dest="."; prev=""
    for a in "$@"; do [ "${prev}" = "--destination" ] && dest="${a}"; prev="${a}"; done
    mkdir -p "${dest}"; : >"${dest}/mock-chart-0.0.1.tgz"
fi
exit 0
EOF

cat >"${BIN}/curl" <<'EOF'
#!/usr/bin/env bash
printf 'curl %s\n' "$*" >>"${ARGV_LOG}"
prev=""
for a in "$@"; do
    [ "${prev}" = "--config" ] && cat "${a}" >>"${CONFIG_LOG}" 2>/dev/null
    prev="${a}"
done
exit 0
EOF
chmod +x "${BIN}/helm" "${BIN}/curl"
export PATH="${BIN}:${PATH}"

WS="${TMP}/ws"; mkdir -p "${WS}/templates"
printf 'apiVersion: v2\nname: mock-chart\nversion: 0.0.1\n' >"${WS}/Chart.yaml"

fail() { echo "FAIL: $1"; echo "--- argv ---"; cat "${ARGV_LOG}" 2>/dev/null; echo "--- config ---"; cat "${CONFIG_LOG}" 2>/dev/null; exit 1; }

run() { # run <backend-env...> : resets logs, runs the entrypoint from the workspace
  : >"${ARGV_LOG}"; : >"${CONFIG_LOG}"
  ( cd "${WS}" && env "$@" bash "${ENTRY}" >/dev/null 2>&1 )
}

assert_no_pw_in_argv() { grep -q 'MARKER' "${ARGV_LOG}" && fail "$1: password leaked into argv"; return 0; }

echo "==> chartmuseum: creds go to a --config file, not argv"
run PLUGIN_BACKEND=chartmuseum PLUGIN_REPO_URL=https://example.invalid/cm \
    PLUGIN_USERNAME=robot PLUGIN_PASSWORD="${PW}"
grep -q -- '--config' "${ARGV_LOG}" || fail "chartmuseum: curl was not given --config"
grep -q 'robot:' "${ARGV_LOG}" && fail "chartmuseum: user:pass pair in argv"
assert_no_pw_in_argv "chartmuseum"
grep -q 'MARKER' "${CONFIG_LOG}" || fail "chartmuseum: password not written to the config file"
grep -q '^user = "robot:' "${CONFIG_LOG}" || fail "chartmuseum: malformed auth config"
[ "$(wc -l <"${CONFIG_LOG}")" -eq 1 ] || fail "chartmuseum: auth config is not a single line (injection?)"

echo "==> nexus: creds go to a --config file, not argv"
run PLUGIN_BACKEND=nexus PLUGIN_REPO_URL=https://example.invalid/nexus \
    PLUGIN_USERNAME=robot PLUGIN_PASSWORD="${PW}"
grep -q -- '--config' "${ARGV_LOG}" || fail "nexus: curl was not given --config"
grep -q 'robot:' "${ARGV_LOG}" && fail "nexus: user:pass pair in argv"
assert_no_pw_in_argv "nexus"
grep -q 'MARKER' "${CONFIG_LOG}" || fail "nexus: password not written to the config file"
grep -q '^user = "robot:' "${CONFIG_LOG}" || fail "nexus: malformed auth config"
[ "$(wc -l <"${CONFIG_LOG}")" -eq 1 ] || fail "nexus: auth config is not a single line (injection?)"

echo "==> oci: password reaches helm via stdin, never argv"
run PLUGIN_BACKEND=oci PLUGIN_OCI_REPO=oci://example.invalid/charts \
    PLUGIN_USERNAME=robot PLUGIN_PASSWORD="${PW}"
grep -q 'registry login' "${ARGV_LOG}" || fail "oci: did not reach helm registry login"
grep -q -- '--password-stdin' "${ARGV_LOG}" || fail "oci: login did not use --password-stdin"
assert_no_pw_in_argv "oci"

echo "==> no-auth (empty username): no --config, no creds anywhere"
run PLUGIN_BACKEND=chartmuseum PLUGIN_REPO_URL=https://example.invalid/cm
grep -q -- '--config' "${ARGV_LOG}" && fail "no-auth: unexpected --config with no username"
[ -s "${CONFIG_LOG}" ] && fail "no-auth: wrote an auth config with no username"

echo "PASS: helm-push entrypoint mock test"
