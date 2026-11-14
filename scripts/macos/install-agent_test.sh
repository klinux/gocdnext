#!/usr/bin/env bash
# Hermetic Linux-side contract test for the macOS installer. macOS-specific
# commands and release downloads are stubbed; the real script still performs
# its validation, checksum comparison, config rendering, and launchctl flow.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
INSTALLER="${HERE}/install-agent.sh"
TEMPLATE="${HERE}/io.gocdnext.agent.plist"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

BIN="${TMP}/bin"
mkdir -p "$BIN"
CALLS="${TMP}/calls.log"
CAPTURE_CONFIG="${TMP}/installed-agent.env"
CAPTURE_MODE="${TMP}/installed-agent.mode"
touch "$CALLS"

cat >"${BIN}/uname" <<'STUB'
#!/usr/bin/env bash
case "${1:-}" in
  -s) printf 'Darwin\n' ;;
  -m) printf 'arm64\n' ;;
  *) printf 'Darwin\n' ;;
esac
STUB

cat >"${BIN}/id" <<'STUB'
#!/usr/bin/env bash
case "${1:-}" in
  -u) printf '501\n' ;;
  -gn) printf 'staff\n' ;;
  *) exit 1 ;;
esac
STUB

cat >"${BIN}/dscl" <<'STUB'
#!/usr/bin/env bash
printf 'NFSHomeDirectory: /Users/builder\n'
STUB

cat >"${BIN}/curl" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
url=""
out=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    http*) url="$1"; shift ;;
    *) shift ;;
  esac
done
[ -n "$url" ] && [ -n "$out" ]
name="${url##*/}"
printf 'curl %s\n' "$name" >>"$MOCK_CALLS"
case "$name" in
  gocdnext-agent_1.2.3_darwin_arm64)
    printf 'mock-mach-o' >"$out"
    ;;
  checksums.txt)
    asset_hash="$(printf 'mock-mach-o' | shasum -a 256 | awk '{ print $1 }')"
    if [ "${MOCK_BAD_ASSET_CHECKSUM:-}" = "1" ]; then
      asset_hash="0000000000000000000000000000000000000000000000000000000000000000"
    fi
    template_hash="$(shasum -a 256 "$MOCK_TEMPLATE" | awk '{ print $1 }')"
    {
      printf '%s  %s\n' "$asset_hash" 'gocdnext-agent_1.2.3_darwin_arm64'
      printf '%s  %s\n' "$template_hash" 'io.gocdnext.agent.plist'
    } >"$out"
    ;;
  checksums.txt.sig|checksums.txt.pem)
    printf 'fixture' >"$out"
    ;;
  *)
    printf 'unexpected curl URL: %s\n' "$url" >&2
    exit 1
    ;;
esac
STUB

cat >"${BIN}/cosign" <<'STUB'
#!/usr/bin/env bash
printf 'cosign' >>"$MOCK_CALLS"
printf ' <%s>' "$@" >>"$MOCK_CALLS"
printf '\n' >>"$MOCK_CALLS"
STUB

# The plist renderer is exercised for call shape here; plistlib validates the
# source template separately in CI. macOS plutil owns key-path replacement.
cat >"${BIN}/plutil" <<'STUB'
#!/usr/bin/env bash
printf 'plutil' >>"$MOCK_CALLS"
printf ' <%s>' "$@" >>"$MOCK_CALLS"
printf '\n' >>"$MOCK_CALLS"
STUB

cat >"${BIN}/install" <<'STUB'
#!/usr/bin/env bash
exit 0
STUB

cat >"${BIN}/launchctl" <<'STUB'
#!/usr/bin/env bash
exit 0
STUB

cat >"${BIN}/sudo" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
printf 'sudo' >>"$MOCK_CALLS"
printf ' <%s>' "$@" >>"$MOCK_CALLS"
printf '\n' >>"$MOCK_CALLS"
if [ "${1:-}" = "install" ] && [ "${!#}" = "/Library/Application Support/gocdnext/agent.env" ]; then
  source_index=$(( $# - 1 ))
  source_path="${!source_index}"
  cp "$source_path" "$MOCK_CAPTURE_CONFIG"
  stat -c '%a' "$source_path" >"$MOCK_CAPTURE_MODE"
fi
STUB

chmod +x "${BIN}"/*

run_installer() {
  local bad_checksum="${1:-0}"
  : >"$CALLS"
  rm -f "$CAPTURE_CONFIG" "$CAPTURE_MODE"
  OUTPUT="$(env \
    PATH="${BIN}:$PATH" \
    MOCK_CALLS="$CALLS" \
    MOCK_TEMPLATE="$TEMPLATE" \
    MOCK_CAPTURE_CONFIG="$CAPTURE_CONFIG" \
    MOCK_CAPTURE_MODE="$CAPTURE_MODE" \
    MOCK_BAD_ASSET_CHECKSUM="$bad_checksum" \
    GOCDNEXT_VERSION=v1.2.3 \
    GOCDNEXT_SERVER_ADDR=grpc.internal:8154 \
    GOCDNEXT_AGENT_NAME=mac-builder-01 \
    GOCDNEXT_AGENT_TOKEN='S3cret-token-value' \
    GOCDNEXT_RUN_USER=builder \
    bash "$INSTALLER" 2>&1)"
  RC=$?
}

fails=0
ok() { printf 'ok: %s\n' "$1"; }
bad() {
  printf 'FAIL: %s\n' "$1"
  [ -n "${2:-}" ] && printf '%s\n' "$2" | tail -40
  fails=$((fails + 1))
}

run_installer 0
calls="$(cat "$CALLS")"
config="$(cat "$CAPTURE_CONFIG" 2>/dev/null || true)"
if [ "$RC" -ne 0 ]; then
  bad "valid signed fixture should install" "$OUTPUT"
elif ! grep -q '^GOCDNEXT_AGENT_TOKEN=S3cret-token-value$' <<<"$config"; then
  bad "token should be written to the private config" "$config"
elif ! grep -q '^GOCDNEXT_AGENT_ENGINE=shell$' <<<"$config"; then
  bad "installer should pin the shell engine" "$config"
elif ! grep -q '^GOCDNEXT_AGENT_TAGS=macos\\,arm64$' <<<"$config"; then
  bad "installer should derive macOS architecture tags" "$config"
elif [ "$(cat "$CAPTURE_MODE" 2>/dev/null)" != "600" ]; then
  bad "temporary credential config should be mode 0600"
elif grep -q 'S3cret-token-value' <<<"${OUTPUT}${calls}"; then
  bad "token leaked to output or command argv" "${OUTPUT}${calls}"
elif ! grep -q 'certificate-identity.*release.yml@refs/tags/v1.2.3' <<<"$calls"; then
  bad "cosign identity should be pinned to the exact release workflow/tag" "$calls"
elif [ "$(grep -n '^cosign' "$CALLS" | cut -d: -f1)" -ge "$(grep -n '^sudo' "$CALLS" | head -1 | cut -d: -f1)" ]; then
  bad "privileged writes must happen only after signature verification" "$calls"
else
  ok "signed arm64 asset -> private shell-agent config -> launchd flow"
fi

run_installer 1
calls="$(cat "$CALLS")"
if [ "$RC" -eq 0 ]; then
  bad "checksum mismatch should fail"
elif ! grep -q 'checksum mismatch' <<<"$OUTPUT"; then
  bad "checksum mismatch should fail precisely" "$OUTPUT"
elif grep -q '^sudo' <<<"$calls"; then
  bad "checksum mismatch must fail before privileged writes" "$calls"
else
  ok "bad checksum fails before sudo"
fi

# The shipped template must restart the daemon on crash / non-zero exit (lost
# connection, panic) but stay DOWN after a clean exit 0 — graceful drain (#178)
# exits 0, so KeepAlive must be {SuccessfulExit:false}, not plain true.
if command -v python3 >/dev/null 2>&1; then
  if python3 - "$TEMPLATE" <<'PY'
import plistlib, sys
with open(sys.argv[1], "rb") as f:
    pl = plistlib.load(f)
ka = pl.get("KeepAlive")
assert ka == {"SuccessfulExit": False}, f"KeepAlive must be {{'SuccessfulExit': False}}, got {ka!r}"
assert pl.get("RunAtLoad") is True, f"RunAtLoad must be true, got {pl.get('RunAtLoad')!r}"
PY
  then
    ok "plist KeepAlive restarts on failure but not on a clean drain exit"
  else
    bad "plist KeepAlive/RunAtLoad shape is wrong"
  fi
else
  printf 'warn: python3 absent, skipping plist KeepAlive assertion\n' >&2
fi

if [ "$fails" -ne 0 ]; then
  printf '%d test(s) failed\n' "$fails" >&2
  exit 1
fi
printf 'all installer tests passed\n'
