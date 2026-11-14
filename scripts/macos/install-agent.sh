#!/bin/bash
# Install the native gocdnext shell agent as a macOS LaunchDaemon.
#
# Required environment (the token deliberately has no CLI flag, so it never
# appears in argv):
#   GOCDNEXT_VERSION, GOCDNEXT_SERVER_ADDR, GOCDNEXT_AGENT_NAME,
#   GOCDNEXT_AGENT_TOKEN
#
# Optional:
#   GOCDNEXT_AGENT_TAGS, GOCDNEXT_AGENT_CAPACITY, GOCDNEXT_RUN_USER,
#   GOCDNEXT_WORKSPACE_ROOT
set -euo pipefail

readonly REPOSITORY="klinux/gocdnext"
readonly SERVICE_LABEL="io.gocdnext.agent"
readonly BINARY_PATH="/usr/local/bin/gocdnext-agent"
readonly WRAPPER_PATH="/usr/local/libexec/gocdnext-agent-wrapper"
readonly SUPPORT_DIR="/Library/Application Support/gocdnext"
readonly CONFIG_PATH="${SUPPORT_DIR}/agent.env"
readonly LOG_DIR="${SUPPORT_DIR}/logs"
readonly STDOUT_LOG="${LOG_DIR}/agent.log"
readonly STDERR_LOG="${LOG_DIR}/agent.err.log"
readonly PLIST_PATH="/Library/LaunchDaemons/${SERVICE_LABEL}.plist"

die() {
  printf 'gocdnext macOS agent install: %s\n' "$*" >&2
  exit 1
}

require_env() {
  local name="$1"
  [ -n "${!name:-}" ] || die "${name} is required"
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "$1 is required"
}

verify_checksum() {
  local published_name="$1"
  local path="$2"
  local expected actual

  expected="$(awk -v name="$published_name" '$2 == name || $2 == "*" name { print $1 }' "${TMP_DIR}/checksums.txt")"
  [ -n "$expected" ] || die "${published_name} is absent from the signed checksums"
  actual="$(shasum -a 256 "$path" | awk '{ print $1 }')"
  [ "$actual" = "$expected" ] || die "checksum mismatch for ${published_name}"
}

[ "$(uname -s)" = "Darwin" ] || die "this installer only runs on macOS"

require_env GOCDNEXT_VERSION
require_env GOCDNEXT_SERVER_ADDR
require_env GOCDNEXT_AGENT_NAME
require_env GOCDNEXT_AGENT_TOKEN

for command_name in awk cosign curl dscl install launchctl plutil sed shasum sudo; do
  require_command "$command_name"
done

case "$(uname -m)" in
  arm64) ARCH="arm64" ;;
  x86_64) ARCH="amd64" ;;
  *) die "unsupported Mac architecture: $(uname -m)" ;;
esac

case "$GOCDNEXT_VERSION" in
  v*) RELEASE_TAG="$GOCDNEXT_VERSION"; VERSION="${GOCDNEXT_VERSION#v}" ;;
  *) RELEASE_TAG="v${GOCDNEXT_VERSION}"; VERSION="$GOCDNEXT_VERSION" ;;
esac

RUN_USER="${GOCDNEXT_RUN_USER:-${SUDO_USER:-$(id -un)}}"
[ "$RUN_USER" != "root" ] || die "set GOCDNEXT_RUN_USER to the unprivileged macOS build account"
id -u "$RUN_USER" >/dev/null 2>&1 || die "macOS account ${RUN_USER} does not exist"
RUN_GROUP="$(id -gn "$RUN_USER")"
RUN_HOME="$(dscl . -read "/Users/${RUN_USER}" NFSHomeDirectory 2>/dev/null | sed 's/^NFSHomeDirectory: //')"
[ -n "$RUN_HOME" ] || die "could not resolve the home directory for ${RUN_USER}"

AGENT_TAGS="${GOCDNEXT_AGENT_TAGS:-macos,${ARCH}}"
AGENT_CAPACITY="${GOCDNEXT_AGENT_CAPACITY:-1}"
case "$AGENT_CAPACITY" in
  ''|*[!0-9]*|0) die "GOCDNEXT_AGENT_CAPACITY must be a positive integer" ;;
esac
WORKSPACE_ROOT="${GOCDNEXT_WORKSPACE_ROOT:-${SUPPORT_DIR}/workspaces}"
case "$WORKSPACE_ROOT" in
  /*) ;;
  *) die "GOCDNEXT_WORKSPACE_ROOT must be an absolute path" ;;
esac

TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/gocdnext-agent.XXXXXX")"
trap 'rm -rf "$TMP_DIR"' EXIT
umask 077

readonly ASSET_NAME="gocdnext-agent_${VERSION}_darwin_${ARCH}"
readonly RELEASE_BASE="https://github.com/${REPOSITORY}/releases/download/${RELEASE_TAG}"

printf 'Downloading gocdnext agent %s for darwin/%s...\n' "$RELEASE_TAG" "$ARCH"
curl --proto '=https' --tlsv1.2 -fsSL \
  "${RELEASE_BASE}/${ASSET_NAME}" -o "${TMP_DIR}/${ASSET_NAME}"
curl --proto '=https' --tlsv1.2 -fsSL \
  "${RELEASE_BASE}/checksums.txt" -o "${TMP_DIR}/checksums.txt"
curl --proto '=https' --tlsv1.2 -fsSL \
  "${RELEASE_BASE}/checksums.txt.sig" -o "${TMP_DIR}/checksums.txt.sig"
curl --proto '=https' --tlsv1.2 -fsSL \
  "${RELEASE_BASE}/checksums.txt.pem" -o "${TMP_DIR}/checksums.txt.pem"

# Pin the signer to this repository's tag workflow; a valid Fulcio certificate
# from an unrelated GitHub Actions workflow is not sufficient.
cosign verify-blob \
  --certificate "${TMP_DIR}/checksums.txt.pem" \
  --signature "${TMP_DIR}/checksums.txt.sig" \
  --certificate-identity "https://github.com/${REPOSITORY}/.github/workflows/release.yml@refs/tags/${RELEASE_TAG}" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
  "${TMP_DIR}/checksums.txt" >/dev/null
verify_checksum "$ASSET_NAME" "${TMP_DIR}/${ASSET_NAME}"

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
TEMPLATE_SOURCE="${SCRIPT_DIR}/io.gocdnext.agent.plist"
if [ ! -f "$TEMPLATE_SOURCE" ]; then
  TEMPLATE_SOURCE="${TMP_DIR}/io.gocdnext.agent.plist"
  curl --proto '=https' --tlsv1.2 -fsSL \
    "${RELEASE_BASE}/io.gocdnext.agent.plist" -o "$TEMPLATE_SOURCE"
fi
verify_checksum "io.gocdnext.agent.plist" "$TEMPLATE_SOURCE"

# The config contains the registration token. It is built without echoing any
# value, installed 0600 outside the workspace, and removed from the temp dir on
# EXIT. The plist and process argv never contain the token.
CONFIG_TMP="${TMP_DIR}/agent.env"
{
  printf 'GOCDNEXT_SERVER_ADDR=%q\n' "$GOCDNEXT_SERVER_ADDR"
  printf 'GOCDNEXT_AGENT_NAME=%q\n' "$GOCDNEXT_AGENT_NAME"
  printf 'GOCDNEXT_AGENT_TOKEN=%q\n' "$GOCDNEXT_AGENT_TOKEN"
  printf 'GOCDNEXT_AGENT_TAGS=%q\n' "$AGENT_TAGS"
  printf 'GOCDNEXT_AGENT_CAPACITY=%q\n' "$AGENT_CAPACITY"
  printf 'GOCDNEXT_AGENT_ENGINE=%q\n' "shell"
  printf 'GOCDNEXT_WORKSPACE_ROOT=%q\n' "$WORKSPACE_ROOT"
} >"$CONFIG_TMP"
chmod 0600 "$CONFIG_TMP"

WRAPPER_TMP="${TMP_DIR}/gocdnext-agent-wrapper"
cat >"$WRAPPER_TMP" <<'WRAPPER'
#!/bin/bash
set -euo pipefail

readonly CONFIG_PATH="${GOCDNEXT_AGENT_CONFIG:-/Library/Application Support/gocdnext/agent.env}"
if [ ! -r "$CONFIG_PATH" ]; then
  printf 'gocdnext agent: config is not readable: %s\n' "$CONFIG_PATH" >&2
  exit 1
fi

set -a
# The file is generated by install-agent.sh, owned by the service account and
# mode 0600. shellcheck source=/dev/null
. "$CONFIG_PATH"
set +a
exec /usr/local/bin/gocdnext-agent
WRAPPER
chmod 0755 "$WRAPPER_TMP"

PLIST_TMP="${TMP_DIR}/${SERVICE_LABEL}.plist"
cp "$TEMPLATE_SOURCE" "$PLIST_TMP"
plutil -replace ProgramArguments.0 -string "$WRAPPER_PATH" "$PLIST_TMP"
plutil -replace UserName -string "$RUN_USER" "$PLIST_TMP"
plutil -replace GroupName -string "$RUN_GROUP" "$PLIST_TMP"
plutil -replace WorkingDirectory -string "$SUPPORT_DIR" "$PLIST_TMP"
plutil -replace EnvironmentVariables.GOCDNEXT_AGENT_CONFIG -string "$CONFIG_PATH" "$PLIST_TMP"
plutil -replace EnvironmentVariables.HOME -string "$RUN_HOME" "$PLIST_TMP"
plutil -replace StandardOutPath -string "$STDOUT_LOG" "$PLIST_TMP"
plutil -replace StandardErrorPath -string "$STDERR_LOG" "$PLIST_TMP"
plutil -lint "$PLIST_TMP" >/dev/null

# Ask for sudo only after downloads/signatures/checksums and local rendering all
# succeed. The long-lived daemon itself is pinned to RUN_USER in the plist.
sudo install -d -m 0755 /usr/local/bin /usr/local/libexec
sudo install -d -m 0750 "$SUPPORT_DIR" "$LOG_DIR" "$WORKSPACE_ROOT"
sudo chown "$RUN_USER:$RUN_GROUP" "$SUPPORT_DIR" "$LOG_DIR" "$WORKSPACE_ROOT"
sudo install -m 0755 -o root -g wheel "${TMP_DIR}/${ASSET_NAME}" "$BINARY_PATH"
sudo install -m 0755 -o root -g wheel "$WRAPPER_TMP" "$WRAPPER_PATH"
sudo install -m 0600 -o "$RUN_USER" -g "$RUN_GROUP" "$CONFIG_TMP" "$CONFIG_PATH"
sudo install -m 0644 -o root -g wheel "$PLIST_TMP" "$PLIST_PATH"

# bootout is intentionally best-effort on a first install. bootstrap/enable/
# kickstart are not: a service that cannot be loaded must fail the installer.
sudo launchctl bootout "system/${SERVICE_LABEL}" >/dev/null 2>&1 || true
sudo launchctl bootstrap system "$PLIST_PATH"
sudo launchctl enable "system/${SERVICE_LABEL}"
sudo launchctl kickstart -k "system/${SERVICE_LABEL}"
sudo launchctl print "system/${SERVICE_LABEL}" >/dev/null

printf 'Installed %s (%s) as %s with tags [%s].\n' \
  "$SERVICE_LABEL" "$RELEASE_TAG" "$RUN_USER" "$AGENT_TAGS"
printf 'Logs: %s and %s\n' "$STDOUT_LOG" "$STDERR_LOG"
