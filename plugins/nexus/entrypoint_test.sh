#!/usr/bin/env bash
# Mock-PATH unit test for the nexus plugin entrypoint. No bats — a plain
# bash harness that stubs `curl` so no network is touched, and asserts the
# LATEST resolution reads maven-metadata.xml over /repository/ and NEVER
# hits the /service/rest search API (which needs the nx-search privilege a
# repo-read CI user lacks — the 401 this change fixes).
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# Stub curl: log the requested URL (last non-flag arg), and
#   - serve maven-metadata.xml for the metadata URL,
#   - write a fake jar to the -o target for a download,
#   - exit non-zero for any /service/rest/ URL (privilege 401) so a
#     regression back to the search API fails loudly.
cat >"$TMP/curl" <<'EOF'
#!/usr/bin/env bash
url="" ; out=""
while [ $# -gt 0 ]; do
    case "$1" in
        -o) out="$2"; shift 2 ;;
        --user|--upload-file) shift 2 ;;
        http://*|https://*) url="$1"; shift ;;
        *) shift ;;
    esac
done
printf '%s\n' "$url" >>"$TMP_LOG"
case "$url" in
    *"/service/rest/"*) echo "mock curl: search API hit (401)" >&2; exit 22 ;;
    *maven-metadata.xml)
        cat <<'XML'
<?xml version="1.0" encoding="UTF-8"?>
<metadata>
  <groupId>br.com.cora.keycloak</groupId>
  <artifactId>cora-condition-flow</artifactId>
  <versioning>
    <latest>1.4.0</latest>
    <release>1.3.0</release>
    <versions>
      <version>1.2.0</version>
      <version>1.3.0</version>
      <version>1.4.0</version>
    </versions>
  </versioning>
</metadata>
XML
        ;;
    *) [ -n "$out" ] && printf 'JARBYTES' >"$out" ;;
esac
EOF
chmod +x "$TMP/curl"

fails=0
pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; fails=1; }

common_env=(
    PLUGIN_ACTION=download
    PLUGIN_URL=https://nexus.test
    PLUGIN_REPOSITORY=maven-repository
    PLUGIN_GROUP_ID=br.com.cora.keycloak
    PLUGIN_ARTIFACT_ID=cora-condition-flow
    PLUGIN_USERNAME=gocd-nexus
    PLUGIN_PASSWORD=secret
)

# The plugin strips a leading "/" from dest to keep it workspace-relative,
# so each case runs from its own workdir with a relative dest.
echo "== 1. LATEST resolves via maven-metadata.xml, downloads <release> =="
log="$TMP/log1"; : >"$log"
wd="$TMP/wd1"; mkdir -p "$wd"
out=$(cd "$wd" && env "${common_env[@]}" PLUGIN_VERSION=LATEST PLUGIN_DEST=libs \
    TMP_LOG="$log" PATH="$TMP:$PATH" bash "$HERE/entrypoint.sh" 2>&1)
rc=$?
[ "$rc" = 0 ] && pass "exit 0" || { fail "exit $rc — $out"; }
grep -q '/repository/maven-repository/br/com/cora/keycloak/cora-condition-flow/maven-metadata.xml$' "$log" \
    && pass "read maven-metadata.xml over /repository/" || fail "metadata URL not requested; log: $(cat "$log")"
# <release> is 1.3.0 (NOT <latest> 1.4.0, which could be a snapshot)
grep -q '/repository/maven-repository/br/com/cora/keycloak/cora-condition-flow/1.3.0/cora-condition-flow-1.3.0.jar$' "$log" \
    && pass "downloaded the <release> version (1.3.0)" || fail "wrong download URL; log: $(cat "$log")"
grep -q '/service/rest/' "$log" && fail "search API was hit — must not be" || pass "no /service/rest call"
[ -s "$wd/libs/cora-condition-flow-1.3.0.jar" ] && pass "jar written to dest" || fail "jar missing in dest"

echo "== 2. explicit version skips metadata, downloads directly =="
log="$TMP/log2"; : >"$log"
wd="$TMP/wd2"; mkdir -p "$wd"
( cd "$wd" && env "${common_env[@]}" PLUGIN_VERSION=1.2.0 PLUGIN_DEST=libs \
    TMP_LOG="$log" PATH="$TMP:$PATH" bash "$HERE/entrypoint.sh" ) >/dev/null 2>&1
grep -q 'maven-metadata.xml' "$log" && fail "pinned version must not read metadata" || pass "no metadata fetch for pinned version"
grep -q '/repository/maven-repository/br/com/cora/keycloak/cora-condition-flow/1.2.0/cora-condition-flow-1.2.0.jar$' "$log" \
    && pass "downloaded the pinned version directly" || fail "wrong pinned URL; log: $(cat "$log")"

if [ "$fails" = 0 ]; then echo "ALL PASS"; else echo "SOME FAILED"; exit 1; fi
