#!/usr/bin/env bash
# Real smoke for the android plugin's IMAGE. Builds the pinned image and runs it
# end to end so a broken SDK / signing wiring fails HERE, before publish. Proves:
#   - the baked SDK is present + usable: sdkmanager lists the pinned platform +
#     build-tools, and apksigner/zipalign/aapt2 are on PATH;
#   - FUNCTIONAL APK signing: a REAL unsigned APK (built offline with aapt2 +
#     the baked android.jar, so it carries a binary manifest with minSdk) is
#     signed by the plugin via apksigner and `apksigner verify` passes — proves
#     the keystore + env-ref password path actually produce a valid v2/v3
#     signature, not merely that apksigner was invoked;
#   - FUNCTIONAL AAB signing: an .aab is signed via jarsigner and `jarsigner
#     -verify` passes;
#   - the signing password NEVER appears in the plugin's log output;
#   - a FAILING build exits non-zero and does NOT sign (no false green);
#   - #361: a glob in the command reaches gradle literally.
#
# Fixtures (keystore, unsigned APK/AAB, stub gradlew) are crafted INSIDE the
# image so keytool/aapt2 come from the image under test; no network at run time.
# Built --no-cache to match a release build. Root-owned workspace outputs are
# cleaned in-image; fail-closed.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMG="gocdnext-plugin-android:smoke-$$"
CNAME="androidsmoke-$$"   # fixed name so a timed-out run's container can be reaped
WAPK="$(mktemp -d)"       # APK + AAB signing project
WFAIL="$(mktemp -d)"      # failing-build project
WGLOB="$(mktemp -d)"      # #361 argv-probe project
SMOKE_OK=0
STOREPASS='smoke-store-pw-7281'   # test keystore password; must never leak to logs

cleanup() {
  docker rm -f "$CNAME" >/dev/null 2>&1 || true
  if docker image inspect "$IMG" >/dev/null 2>&1; then
    docker run --rm --entrypoint bash -v "$WAPK:/a" -v "$WFAIL:/b" -v "$WGLOB:/c" "$IMG" \
      -c 'for d in /a /b /c; do rm -rf "$d"/* "$d"/.[!.]* "$d"/..?* 2>/dev/null || true; done' \
      >/dev/null 2>&1 || true
  fi
  docker image rm -f "$IMG" >/dev/null 2>&1 || true
  rm -rf "$WAPK" "$WFAIL" "$WGLOB" 2>/dev/null || true
  leftover=0
  for d in "$WAPK" "$WFAIL" "$WGLOB"; do
    [ -d "$d" ] && { echo "WARN: cleanup could not fully remove ${d}" >&2; leftover=1; }
  done
  docker image inspect "$IMG" >/dev/null 2>&1 && { echo "WARN: smoke image not removed" >&2; leftover=1; }
  [ "$leftover" = 1 ] && [ "$SMOKE_OK" = 1 ] && { echo "FAIL: smoke passed but left state behind" >&2; exit 1; }
  return 0
}
trap cleanup EXIT
die() { echo "FAIL: $1"; [ -n "${2:-}" ] && { echo "--- output ---"; echo "$2" | tail -40; }; exit 1; }

# Hard wall-clock cap with TERM->KILL escalation so a hung run is bounded
# (RC=124 on TERM, RC=137 on forced KILL). Container is named so a timed-out,
# --rm-stranded one is reaped in cleanup.
run() { d="$1"; shift; docker rm -f "$CNAME" >/dev/null 2>&1 || true; set +e; OUT="$(timeout --kill-after=20s 420s docker run --rm --name "$CNAME" --network none -v "$d:/workspace" -w /workspace "$@" "$IMG" 2>&1)"; RC=$?; set -e; }
# in-image helper: run an arbitrary bash snippet against a mounted workspace.
inimg() { d="$1"; shift; docker run --rm --network none --entrypoint bash -v "$d:/workspace" -w /workspace "$IMG" -c "$1"; }

echo "==> build plugin image (--no-cache, same Dockerfile/context as the official build)"
docker build --no-cache -t "$IMG" "$HERE" >/dev/null

echo "==> baked SDK is present + usable (pinned platform/build-tools; apksigner/zipalign/aapt2 on PATH)"
SDK="$(inimg "$WAPK" 'sdkmanager --list_installed 2>/dev/null')" || die "sdkmanager --list_installed failed" "$SDK"
grep -q 'platforms;android-36' <<<"$SDK" || die "baked platform android-36 missing" "$SDK"
grep -q 'build-tools;36.0.0'   <<<"$SDK" || die "baked build-tools 36.0.0 missing" "$SDK"
inimg "$WAPK" 'command -v apksigner && command -v zipalign && command -v aapt2 && apksigner --version' >/dev/null \
  || die "signing tools not on PATH"

# Craft fixtures INSIDE the image: keystore (keytool), a REAL unsigned APK
# (aapt2 link against the baked android.jar → binary manifest w/ minSdk), a
# dummy AAB (zip), and a stub ./gradlew that exits 0 so the entrypoint's build
# step "passes" and proceeds to sign (gradle itself is covered by its own plugin).
echo "==> craft signing fixtures (keystore + real unsigned APK + AAB) in-image"
inimg "$WAPK" "
  set -e
  keytool -genkeypair -keystore ks.jks -storepass '$STOREPASS' -keypass '$STOREPASS' \
    -alias upload -keyalg RSA -keysize 2048 -validity 30 -dname 'CN=gocdnext smoke' >/dev/null 2>&1
  base64 -w0 ks.jks > ks.b64
  cat > AndroidManifest.xml <<'MANIFEST'
<?xml version=\"1.0\" encoding=\"utf-8\"?>
<manifest xmlns:android=\"http://schemas.android.com/apk/res/android\" package=\"com.example.smoke\">
  <uses-sdk android:minSdkVersion=\"21\" android:targetSdkVersion=\"36\" />
  <application android:label=\"smoke\" />
</manifest>
MANIFEST
  mkdir -p app/build/outputs/apk/release app/build/outputs/bundle/release
  aapt2 link -o app/build/outputs/apk/release/app-release.apk \
    -I \$ANDROID_HOME/platforms/android-36/android.jar \
    --manifest AndroidManifest.xml --version-code 1 --version-name 1.0
  # Build the dummy .aab with 'jar' (ships with the JDK) — the jdk-base image
  # has 'unzip' but not 'zip', and the plugin itself needs neither at runtime.
  (cd app/build/outputs/bundle/release && echo 'x' > base.txt && jar cf app-release.aab base.txt && rm -f base.txt)
  printf '#!/bin/sh\nexit 0\n' > gradlew && chmod +x gradlew
  rm -f ks.jks AndroidManifest.xml
"
KSB64="$(cat "$WAPK/ks.b64")"
[ -n "$KSB64" ] || die "keystore base64 fixture was not produced"

echo "==> plugin signs the APK (apksigner) and AAB (jarsigner); password stays off the log"
run "$WAPK" \
  -e PLUGIN_COMMAND=assembleRelease \
  -e PLUGIN_KEYSTORE="$KSB64" \
  -e PLUGIN_KEYSTORE_PASSWORD="$STOREPASS" \
  -e PLUGIN_KEY_ALIAS=upload
case "$RC" in
  124|137) die "signing run hung (watchdog RC=$RC)" "$OUT" ;;
esac
[ "$RC" -eq 0 ] || die "signing run: expected exit 0, got $RC" "$OUT"
grep -q 'signed + verified' <<<"$OUT" || die "signing run: apk sign+verify marker missing" "$OUT"
grep -qF "$STOREPASS" <<<"$OUT" && die "SECRET LEAK: keystore password appeared in the plugin log" "$OUT"

echo "==> independently verify the produced signatures (apksigner verify / jarsigner -verify)"
inimg "$WAPK" 'apksigner verify --verbose app/build/outputs/apk/release/app-release.apk' >/dev/null \
  || die "apksigner verify failed on the plugin-signed APK"
VJ="$(inimg "$WAPK" 'jarsigner -verify app/build/outputs/bundle/release/app-release.aab')" \
  || die "jarsigner -verify failed on the plugin-signed AAB" "$VJ"
grep -q 'jar verified' <<<"$VJ" || die "AAB not reported as verified by jarsigner" "$VJ"

# Stub gradlew scripts are pure shell — write them on the HOST (into the mounted
# workspace) to avoid fragile nested escaping through docker -c.
echo "==> a FAILING build exits non-zero and does NOT sign"
printf '#!/bin/sh\nexit 7\n' > "$WFAIL/gradlew"; chmod +x "$WFAIL/gradlew"
run "$WFAIL" -e PLUGIN_COMMAND=assembleRelease -e PLUGIN_KEYSTORE="$KSB64" \
  -e PLUGIN_KEYSTORE_PASSWORD="$STOREPASS" -e PLUGIN_KEY_ALIAS=upload
[ "$RC" -eq 7 ] || die "failing build: expected the build's exit 7 to propagate, got $RC" "$OUT"
grep -q 'apksigner' <<<"$OUT" && die "failing build must not reach signing" "$OUT"

echo "==> #361: a glob in PLUGIN_COMMAND reaches gradle literally"
cat > "$WGLOB/gradlew" <<'STUB'
#!/bin/sh
echo GRADLEW_ARGV_BEGIN
for a in "$@"; do printf 'ARG=%s\n' "$a"; done
echo GRADLEW_ARGV_END
STUB
chmod +x "$WGLOB/gradlew"
: > "$WGLOB/decoy-a.txt"; : > "$WGLOB/decoy-b.txt"
run "$WGLOB" -e PLUGIN_COMMAND='build *.txt'
[ "$RC" -eq 0 ] || die "argv probe: expected exit 0, got $RC" "$OUT"
AV="$(awk '/GRADLEW_ARGV_BEGIN/{f=1;next}/GRADLEW_ARGV_END/{f=0}f' <<<"$OUT")"
grep -qxF 'ARG=*.txt' <<<"$AV" || die "PLUGIN_COMMAND glob-expanded against the workspace" "$AV"
grep -qxF 'ARG=decoy-a.txt' <<<"$AV" && die "PLUGIN_COMMAND glob-expanded (decoy leaked into argv)" "$AV"

SMOKE_OK=1
echo "PASS: android smoke"
