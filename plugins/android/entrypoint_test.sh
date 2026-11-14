#!/usr/bin/env bash
# Hermetic unit tests for the android plugin entrypoint. Runs on the CI host
# (no image, no real SDK): select-jdk + sdkmanager + gradle + apksigner +
# jarsigner + zipalign are stubbed on PATH. Proves the entrypoint's validation,
# secret handling (passwords NEVER on argv), artifact auto-detection, and glob
# safety — the real image build + functional signature are covered by
# smoke_test.sh.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
REAL_SELECT_JDK="$REPO/plugins/jdk-base/select-jdk.sh"
ENTRY="$HERE/entrypoint.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fails=0
ok()   { echo "ok: $1"; }
bad()  { echo "FAIL: $1"; [ -n "${2:-}" ] && { echo "--- output ---"; printf '%s\n' "$2" | tail -30; }; fails=$((fails+1)); }

# --- stubs ----------------------------------------------------------------
BIN="$TMP/bin"; mkdir -p "$BIN"
LOG="$TMP/calls.log"

# no-op JDK selector for the valid-path cases
NOOP_JDK="$TMP/select-noop.sh"; printf ':\n' > "$NOOP_JDK"

cat >"$BIN/gradle" <<EOF
#!/usr/bin/env bash
echo "GRADLE_ARGV_BEGIN"
for a in "\$@"; do printf 'ARG=%s\n' "\$a"; done
echo "GRADLE_ARGV_END"
exit \${MOCK_GRADLE_RC:-0}
EOF
cat >"$BIN/sdkmanager" <<EOF
#!/usr/bin/env bash
# Exit immediately (don't drain the infinite 'yes' — that would hang). The
# entrypoint runs 'yes | sdkmanager' with pipefail off and reads PIPESTATUS.
echo "sdkmanager \$*" >> "$LOG"
exit \${MOCK_SDK_RC:-0}
EOF
cat >"$BIN/apksigner" <<EOF
#!/usr/bin/env bash
echo "apksigner \$*" >> "$LOG"
exit 0
EOF
cat >"$BIN/jarsigner" <<EOF
#!/usr/bin/env bash
echo "jarsigner \$*" >> "$LOG"
exit 0
EOF
cat >"$BIN/zipalign" <<EOF
#!/usr/bin/env bash
echo "zipalign \$*" >> "$LOG"
out="\${@: -1}"; : > "\$out"   # produce the .aligned output the entrypoint mv's
exit 0
EOF
chmod +x "$BIN"/*

# run <workdir> [env KEY=VAL ...] — invokes the entrypoint with stubs on PATH.
run() {
    local wd="$1"; shift
    : > "$LOG"
    OUT="$( cd "$wd" && env PATH="$BIN:$PATH" SELECT_JDK="${SELECT_JDK_OVERRIDE:-$NOOP_JDK}" "$@" bash "$ENTRY" 2>&1 )"
    RC=$?
}
newwd() { local d; d="$(mktemp -d "$TMP/wd.XXXXXX")"; printf '%s' "$d"; }

# A valid, non-empty base64 keystore blob (decodes to bytes; content irrelevant
# to the stubbed signers).
KS_B64="$(printf 'gocdnext-test-keystore-bytes' | base64 | tr -d '\n')"
SECRET_PW='SuperSecretPw123!'

# === 1. command required ==================================================
run "$(newwd)" PLUGIN_COMMAND=""
{ [ "$RC" -eq 2 ] && grep -q 'PLUGIN_COMMAND is required' <<<"$OUT"; } \
    && ok "missing command → exit 2" || bad "missing command should exit 2" "$OUT"

# === 2. invalid jdk fails loud (forwarded to select-jdk) ==================
SELECT_JDK_OVERRIDE="$REAL_SELECT_JDK" run "$(newwd)" PLUGIN_COMMAND=assembleRelease PLUGIN_JDK=211
{ [ "$RC" -eq 2 ] && grep -q "jdk: must be 11, 17, 21, or 25" <<<"$OUT"; } \
    && ok "invalid jdk → exit 2" || bad "invalid jdk should exit 2" "$OUT"
unset SELECT_JDK_OVERRIDE

# === 3. plain build, no signing ===========================================
run "$(newwd)" PLUGIN_COMMAND=assembleRelease
{ [ "$RC" -eq 0 ] && grep -q 'ARG=assembleRelease' <<<"$OUT" && grep -q 'done' <<<"$OUT"; } \
    && ok "plain build runs gradle, exit 0" || bad "plain build should run gradle" "$OUT"

# === 4. build failure propagates, signing skipped =========================
run "$(newwd)" PLUGIN_COMMAND=assembleRelease MOCK_GRADLE_RC=1 PLUGIN_KEYSTORE="$KS_B64" \
    PLUGIN_KEYSTORE_PASSWORD="$SECRET_PW" PLUGIN_KEY_ALIAS=upload
{ [ "$RC" -eq 1 ] && ! grep -q 'apksigner' "$LOG"; } \
    && ok "build failure → exit 1, no signing" || bad "build failure should exit 1 and skip signing" "$OUT"

# === 5. APK signing: apksigner + zipalign, password NEVER on argv =========
wd="$(newwd)"; mkdir -p "$wd/app/build/outputs/apk/release"; : > "$wd/app/build/outputs/apk/release/app-release.apk"
run "$wd" PLUGIN_COMMAND=assembleRelease PLUGIN_KEYSTORE="$KS_B64" \
    PLUGIN_KEYSTORE_PASSWORD="$SECRET_PW" PLUGIN_KEY_ALIAS=upload
calls="$(cat "$LOG")"
if [ "$RC" -ne 0 ]; then bad "apk signing should exit 0" "$OUT"
elif ! grep -q 'zipalign ' <<<"$calls"; then bad "apk signing should zipalign" "$calls"
elif ! grep -q 'apksigner sign' <<<"$calls"; then bad "apk signing should call apksigner sign" "$calls"
elif ! grep -q 'env:PLUGIN_KEYSTORE_PASSWORD' <<<"$calls"; then bad "apksigner should take password via env: ref" "$calls"
elif grep -qF "$SECRET_PW" <<<"$calls"; then bad "SECRET LEAK: password value appeared on apksigner argv" "$calls"
elif grep -qF "$SECRET_PW" <<<"$OUT"; then bad "SECRET LEAK: password value appeared in entrypoint output" "$OUT"
else ok "apk signed via apksigner, password off-argv"; fi

# === 6. AAB signing: jarsigner, password NEVER on argv ====================
wd="$(newwd)"; mkdir -p "$wd/app/build/outputs/bundle/release"; : > "$wd/app/build/outputs/bundle/release/app-release.aab"
run "$wd" PLUGIN_COMMAND=bundleRelease PLUGIN_KEYSTORE="$KS_B64" \
    PLUGIN_KEYSTORE_PASSWORD="$SECRET_PW" PLUGIN_KEY_ALIAS=upload
calls="$(cat "$LOG")"
if [ "$RC" -ne 0 ]; then bad "aab signing should exit 0" "$OUT"
elif ! grep -q 'jarsigner ' <<<"$calls"; then bad "aab signing should call jarsigner" "$calls"
elif ! grep -q -- '-storepass:env PLUGIN_KEYSTORE_PASSWORD' <<<"$calls"; then bad "jarsigner should take storepass via :env ref" "$calls"
elif grep -qF "$SECRET_PW" <<<"$calls"; then bad "SECRET LEAK: password value appeared on jarsigner argv" "$calls"
else ok "aab signed via jarsigner, password off-argv"; fi

# === 7. keystore set but password missing → exit 2 ========================
wd="$(newwd)"; mkdir -p "$wd/app/build/outputs/apk/release"; : > "$wd/app/build/outputs/apk/release/app-release.apk"
run "$wd" PLUGIN_COMMAND=assembleRelease PLUGIN_KEYSTORE="$KS_B64" PLUGIN_KEY_ALIAS=upload
{ [ "$RC" -eq 2 ] && grep -q 'keystore-password is required' <<<"$OUT"; } \
    && ok "keystore without password → exit 2" || bad "keystore without password should exit 2" "$OUT"

# === 8. signing requested but no artifacts found → exit 1 =================
run "$(newwd)" PLUGIN_COMMAND=assembleRelease PLUGIN_KEYSTORE="$KS_B64" \
    PLUGIN_KEYSTORE_PASSWORD="$SECRET_PW" PLUGIN_KEY_ALIAS=upload
{ [ "$RC" -eq 1 ] && grep -q 'no .apk/.aab found' <<<"$OUT"; } \
    && ok "no artifacts to sign → exit 1" || bad "no artifacts should exit 1" "$OUT"

# === 8b. explicit sign-paths matching nothing → exit 1 (no silent unsigned) ===
# A real artifact exists under auto-detect, but explicit sign-paths overrides it
# and points at a missing literal path → nothing signed must FAIL, not succeed.
wd="$(newwd)"; mkdir -p "$wd/app/build/outputs/apk/release"; : > "$wd/app/build/outputs/apk/release/app-release.apk"
run "$wd" PLUGIN_COMMAND=assembleRelease PLUGIN_KEYSTORE="$KS_B64" \
    PLUGIN_KEYSTORE_PASSWORD="$SECRET_PW" PLUGIN_KEY_ALIAS=upload PLUGIN_SIGN_PATHS='does-not-exist.apk'
{ [ "$RC" -eq 1 ] && grep -q 'no artifact was signed' <<<"$OUT"; } \
    && ok "explicit sign-paths matching nothing → exit 1" || bad "explicit sign-paths matching nothing should exit 1" "$OUT"

# === 9. invalid base64 keystore → exit 2 ==================================
wd="$(newwd)"; mkdir -p "$wd/app/build/outputs/apk/release"; : > "$wd/app/build/outputs/apk/release/app-release.apk"
run "$wd" PLUGIN_COMMAND=assembleRelease PLUGIN_KEYSTORE='%%%not-base64%%%' \
    PLUGIN_KEYSTORE_PASSWORD="$SECRET_PW" PLUGIN_KEY_ALIAS=upload
{ [ "$RC" -eq 2 ] && grep -q 'not valid base64' <<<"$OUT"; } \
    && ok "invalid base64 keystore → exit 2" || bad "invalid base64 keystore should exit 2" "$OUT"

# === 10. sdk-components forwarded to sdkmanager ===========================
run "$(newwd)" PLUGIN_COMMAND=assembleRelease PLUGIN_SDK_COMPONENTS='ndk;26.1.10909125 platforms;android-34'
{ [ "$RC" -eq 0 ] && grep -q 'sdkmanager ndk;26.1.10909125 platforms;android-34' "$LOG"; } \
    && ok "sdk-components forwarded to sdkmanager" || bad "sdk-components should reach sdkmanager" "$(cat "$LOG")"

# === 11. sdkmanager failure → exit 1 ======================================
run "$(newwd)" PLUGIN_COMMAND=assembleRelease PLUGIN_SDK_COMPONENTS='bogus;component' MOCK_SDK_RC=1
{ [ "$RC" -eq 1 ] && grep -q 'sdkmanager failed' <<<"$OUT"; } \
    && ok "sdkmanager failure → exit 1" || bad "sdkmanager failure should exit 1" "$OUT"

# === 12. #361 glob in PLUGIN_COMMAND reaches gradle literally =============
wd="$(newwd)"; : > "$wd/decoy-a.gradle"; : > "$wd/decoy-b.gradle"
run "$wd" PLUGIN_COMMAND='build *.gradle'
argv="$(awk '/GRADLE_ARGV_BEGIN/{f=1;next}/GRADLE_ARGV_END/{f=0}f' <<<"$OUT")"
if ! grep -qxF 'ARG=*.gradle' <<<"$argv"; then bad "PLUGIN_COMMAND glob-expanded (literal '*.gradle' did not reach gradle)" "$argv"
elif grep -qxF 'ARG=decoy-a.gradle' <<<"$argv"; then bad "PLUGIN_COMMAND glob-expanded (decoy leaked into argv)" "$argv"
else ok "#361: glob in command stays literal"; fi

# === 13. #361 glob in PLUGIN_ARGS reaches gradle literally ================
wd="$(newwd)"; : > "$wd/decoy-a.gradle"
run "$wd" PLUGIN_COMMAND='build' PLUGIN_ARGS='*.gradle'
argv="$(awk '/GRADLE_ARGV_BEGIN/{f=1;next}/GRADLE_ARGV_END/{f=0}f' <<<"$OUT")"
if ! grep -qxF 'ARG=*.gradle' <<<"$argv"; then bad "PLUGIN_ARGS glob-expanded (literal did not reach gradle)" "$argv"
elif grep -qxF 'ARG=decoy-a.gradle' <<<"$argv"; then bad "PLUGIN_ARGS glob-expanded (decoy leaked into argv)" "$argv"
else ok "#361: glob in args stays literal"; fi

# === 14. tri-state + console typo fails loud ==============================
run "$(newwd)" PLUGIN_COMMAND=assembleRelease PLUGIN_CONSOLE=fancy
{ [ "$RC" -eq 2 ] && grep -q 'console accepts' <<<"$OUT"; } \
    && ok "console typo → exit 2" || bad "console typo should exit 2" "$OUT"

echo "----"
if [ "$fails" -ne 0 ]; then echo "FAILED: $fails test(s)"; exit 1; fi
echo "PASS: android entrypoint"
