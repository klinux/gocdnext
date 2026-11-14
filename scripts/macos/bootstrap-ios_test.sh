#!/usr/bin/env bash
# Hermetic Linux-side contract test. macOS, Xcode, Homebrew, RubyGems and the
# installed commands are stubs; the real bootstrap still drives all decisions.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
BOOTSTRAP="${HERE}/bootstrap-ios.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

BIN="${TMP}/bin"
HOME_DIR="${TMP}/home"
DEVELOPER_DIR="${TMP}/Xcode.app/Contents/Developer"
RUBY_PREFIX="${TMP}/brew/opt/ruby@3.4"
CALLS="${TMP}/calls.log"
XCODE_SELECTED="${TMP}/xcode-selected"
XCODE_READY="${TMP}/xcode-ready"
BREW_INSTALLED="${TMP}/brew-installed"
GEMS="${TMP}/gems"
mkdir -p "$BIN" "$HOME_DIR" "${DEVELOPER_DIR}/usr/bin" "${RUBY_PREFIX}/bin"
touch "$CALLS"

cat >"${BIN}/uname" <<'STUB'
#!/usr/bin/env bash
[ "${1:-}" = "-s" ] && printf 'Darwin\n' || printf 'arm64\n'
STUB

cat >"${BIN}/xcode-select" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  -p)
    if [ -f "$MOCK_XCODE_SELECTED" ]; then cat "$MOCK_XCODE_SELECTED"; else printf '/Library/Developer/CommandLineTools\n'; fi
    ;;
  --switch)
    printf '%s\n' "$2" >"$MOCK_XCODE_SELECTED"
    printf 'xcode-select <%s> <%s>\n' "$1" "$2" >>"$MOCK_CALLS"
    ;;
  *) exit 2 ;;
esac
STUB

cat >"${DEVELOPER_DIR}/usr/bin/xcodebuild" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
printf 'xcodebuild' >>"$MOCK_CALLS"
printf ' <%s>' "$@" >>"$MOCK_CALLS"
printf '\n' >>"$MOCK_CALLS"
case "${1:-}" in
  -checkFirstLaunchStatus) [ -f "$MOCK_XCODE_READY" ] ;;
  -runFirstLaunch) touch "$MOCK_XCODE_READY" ;;
  -license) [ "${2:-}" = "accept" ] ;;
  -version) printf 'Xcode 18.1\nBuild version 22B1\n' ;;
  *) exit 2 ;;
esac
STUB

cat >"${BIN}/sudo" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
printf 'sudo' >>"$MOCK_CALLS"
printf ' <%s>' "$@" >>"$MOCK_CALLS"
printf '\n' >>"$MOCK_CALLS"
"$@"
STUB

cat >"${BIN}/brew" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  list) [ -f "$MOCK_BREW_INSTALLED" ] ;;
  install)
    printf 'brew install <%s>\n' "$2" >>"$MOCK_CALLS"
    touch "$MOCK_BREW_INSTALLED"
    ;;
  --prefix) printf '%s\n' "$MOCK_RUBY_PREFIX" ;;
  *) exit 2 ;;
esac
STUB

cat >"${RUBY_PREFIX}/bin/gem" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  list)
    name="${5:-}"; version="${7:-}"
    [ -f "${MOCK_GEMS}/${name}-${version}" ]
    ;;
  install)
    name="$2"; version="$4"; bindir="$7"
    printf 'gem install <%s> <%s>\n' "$name" "$version" >>"$MOCK_CALLS"
    mkdir -p "$MOCK_GEMS" "$bindir"
    touch "${MOCK_GEMS}/${name}-${version}"
    case "$name" in
      bundler) command=bundle; output="Bundler version ${version}" ;;
      fastlane) command=fastlane; output="fastlane ${version}" ;;
      cocoapods) command=pod; output="${version}" ;;
      *) exit 2 ;;
    esac
    printf '#!/usr/bin/env bash\nprintf '\''%%s\\n'\'' %q\n' "$output" >"${bindir}/${command}"
    chmod +x "${bindir}/${command}"
    ;;
  *) exit 2 ;;
esac
STUB

cat >"${RUBY_PREFIX}/bin/ruby" <<'STUB'
#!/usr/bin/env bash
if [ "${1:-}" = "-rrubygems" ]; then
  printf '%s/lib/ruby/gems/3.4.0\n' "$MOCK_RUBY_PREFIX"
else
  printf 'ruby 3.4.7 mock\n'
fi
STUB

cat >"${BIN}/xcodebuild" <<'STUB'
#!/usr/bin/env bash
exec "$MOCK_DEVELOPER_DIR/usr/bin/xcodebuild" "$@"
STUB

chmod +x "${BIN}"/* "${DEVELOPER_DIR}/usr/bin/xcodebuild" "${RUBY_PREFIX}/bin/"*

run_bootstrap() {
  OUTPUT="$(env \
    PATH="${BIN}:/usr/bin:/bin" \
    HOME="$HOME_DIR" \
    MOCK_CALLS="$CALLS" \
    MOCK_XCODE_SELECTED="$XCODE_SELECTED" \
    MOCK_XCODE_READY="$XCODE_READY" \
    MOCK_BREW_INSTALLED="$BREW_INSTALLED" \
    MOCK_RUBY_PREFIX="$RUBY_PREFIX" \
    MOCK_GEMS="$GEMS" \
    MOCK_DEVELOPER_DIR="$DEVELOPER_DIR" \
    GOCDNEXT_XCODE_DEVELOPER_DIR="$DEVELOPER_DIR" \
    bash "$BOOTSTRAP" 2>&1)"
  RC=$?
}

fails=0
ok() { printf 'ok: %s\n' "$1"; }
bad() { printf 'FAIL: %s\n%s\n' "$1" "${2:-}"; fails=$((fails + 1)); }

run_bootstrap
env_file="${HOME_DIR}/.config/gocdnext/ios-toolchain.env"
if [ "$RC" -ne 0 ]; then
  bad "first bootstrap should succeed" "$OUTPUT"
elif [ "$(stat -c '%a' "$env_file")" != "600" ]; then
  bad "toolchain environment must be mode 0600"
elif [ "$(grep -c '^gem install' "$CALLS")" -ne 3 ]; then
  bad "the three pinned Ruby tools should be installed once" "$(cat "$CALLS")"
elif ! grep -q 'sudo.*xcode-select.*--switch' "$CALLS" || \
     ! grep -q 'sudo.*xcodebuild.*-license.*accept' "$CALLS" || \
     ! grep -q 'sudo.*xcodebuild.*-runFirstLaunch' "$CALLS"; then
  bad "Xcode selection, license and first-launch setup must be explicit" "$(cat "$CALLS")"
elif ! grep -q 'fastlane: fastlane 2.241.1' <<<"$OUTPUT"; then
  bad "bootstrap should verify the pinned toolchain" "$OUTPUT"
else
  ok "first run selects Xcode and installs the pinned user toolchain"
fi

before_mutations="$(grep -E '^(sudo|brew install|gem install)' "$CALLS" || true)"
run_bootstrap
after_mutations="$(grep -E '^(sudo|brew install|gem install)' "$CALLS" || true)"
if [ "$RC" -ne 0 ]; then
  bad "second bootstrap should succeed" "$OUTPUT"
elif [ "$before_mutations" != "$after_mutations" ]; then
  bad "second run must not reinstall or repeat privileged Xcode setup" "$(cat "$CALLS")"
else
  ok "second run is idempotent"
fi

if [ "$fails" -ne 0 ]; then
  printf '%d test(s) failed\n' "$fails" >&2
  exit 1
fi
printf 'all iOS bootstrap tests passed\n'
