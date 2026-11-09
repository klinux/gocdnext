#!/usr/bin/env bash
# Real smoke for the sonar plugin's IMAGE. The plugin is a scanner front-end for
# SonarQube/SonarCloud, so a real scan needs a server — out of scope offline.
# What IS provable offline, and what the bouncycastle fix (CVE-2025-14813 +
# CVE-2026-8763) and the Gradle bump actually risk, is proved here:
#   - the bundled Gradle's BC suite (bcpg/bcprov/bcutil) is a single version
#     >= 1.85, no 1.78.1 survivor — the jars Gradle loads for PGP/signing;
#   - that swapped/bumped crypto is FUNCTIONAL, not merely present: the bundled
#     `gradle` runs a real build that applies the core `signing` plugin and
#     PGP-signs a file (exercises bcpg+bcprov; a broken jar throws when the first
#     BC class loads, which a version check alone would miss);
#   - the three scanner front-ends survived the bump and report versions
#     (gradle 8.14.6, sonar-scanner CLI, maven);
#   - the entrypoint's auth contract holds: no token -> fail fast, exit 2.
#
# Offline (`--network none`); the signing key is generated per run via gpg and
# never committed. Built --no-cache to match a release build. Root-owned
# workspace outputs are cleaned in-image; fail-closed.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMG="gocdnext-plugin-sonar:smoke-$$"
CNAME="sonarsmoke-$$"   # fixed name so a timed-out run's container can be reaped
WOK="$(mktemp -d)"      # signing project
SMOKE_OK=0

cleanup() {
  # Kill any stuck run container FIRST: a timed-out one still holds $WOK mounted,
  # so the mount-cleaning container below would otherwise race a live writer.
  docker rm -f "$CNAME" >/dev/null 2>&1 || true
  if docker image inspect "$IMG" >/dev/null 2>&1; then
    docker run --rm --entrypoint bash -v "$WOK:/a" "$IMG" \
      -c 'rm -rf /a/* /a/.[!.]* /a/..?* 2>/dev/null || true' >/dev/null 2>&1 || true
  fi
  docker image rm -f "$IMG" >/dev/null 2>&1 || true
  rm -rf "$WOK" 2>/dev/null || true
  leftover=0
  [ -d "$WOK" ] && { echo "WARN: cleanup could not fully remove ${WOK}" >&2; leftover=1; }
  docker image inspect "$IMG" >/dev/null 2>&1 && { echo "WARN: smoke image not removed" >&2; leftover=1; }
  [ "$leftover" = 1 ] && [ "$SMOKE_OK" = 1 ] && { echo "FAIL: smoke passed but left state behind" >&2; exit 1; }
  return 0
}
trap cleanup EXIT
die() { echo "FAIL: $1"; [ -n "${2:-}" ] && { echo "--- output ---"; echo "$2" | tail -40; }; exit 1; }

command -v gpg >/dev/null 2>&1 || die "gpg is required to generate the disposable signing key"

# Disposable, unprotected PGP key, generated per run into an isolated GNUPGHOME —
# NEVER committed.
gen_signing_key() {
  local gh; gh="$(mktemp -d)"
  GNUPGHOME="$gh" gpg --batch --gen-key >/dev/null 2>&1 <<'PARAMS'
%no-protection
Key-Type: RSA
Key-Length: 2048
Name-Real: gocdnext sonar smoke
Name-Email: smoke@example.invalid
Expire-Date: 0
%commit
PARAMS
  GNUPGHOME="$gh" gpg --batch --armor --export-secret-keys smoke@example.invalid 2>/dev/null
  rm -rf "$gh"
}

project_sign() {
  local d="$1"
  gen_signing_key > "$d/signing-key.asc"
  [ -s "$d/signing-key.asc" ] || die "failed to generate signing key"
  cat >"$d/settings.gradle" <<'G'
rootProject.name = 'bc-smoke'
G
  cat >"$d/build.gradle" <<'G'
plugins { id 'signing' }
def secretKey = file('signing-key.asc').text
signing { useInMemoryPgpKeys(secretKey, '') }
tasks.register('gocdnextSign') {
  doLast {
    def payload = new File(projectDir, 'payload.txt')
    payload.text = 'gocdnext-bc-smoke'
    signing.sign(payload)            // bcpg/bcprov do the work here
    def sig = new File(projectDir, 'payload.txt.asc')
    if (!sig.exists()) { throw new GradleException('signature .asc was not produced') }
    if (!sig.text.contains('BEGIN PGP SIGNATURE')) { throw new GradleException('output is not a PGP signature') }
    println 'GOCDNEXT_BC_SIGN_OK'
  }
}
G
}

# Hard wall-clock cap with TERM->KILL escalation (RC=124 TERM, RC=137 KILL) around
# the given command. 300s: a cold --no-daemon JVM + signing, offline.
run() { set +e; OUT="$(timeout --kill-after=15s 300s "$@" 2>&1)"; RC=$?; set -e; }

echo "==> build plugin image (--no-cache, same Dockerfile/context as the official build)"
docker build --no-cache -t "$IMG" "$HERE" >/dev/null

echo "==> bundled gradle's bouncycastle suite is a single version >= 1.85, no 1.78.1"
BCS="$(docker run --rm --network none --entrypoint sh "$IMG" -c 'ls -1 /opt/gradle/lib/plugins/ 2>/dev/null | grep -E "^bc(pg|prov|util)-jdk18on-[0-9]"')" \
  || die "could not list bouncycastle jars" "$BCS"
[ "$(printf '%s\n' "$BCS" | grep -c .)" -eq 3 ] || die "expected exactly 3 BC jars (bcpg/bcprov/bcutil)" "$BCS"
printf '%s\n' "$BCS" | grep -q -- '-1.78.1.jar' && die "a vulnerable BC 1.78.1 jar survived" "$BCS"
UNIQ_VERS="$(printf '%s\n' "$BCS" | grep -oE '[0-9]+\.[0-9]+(\.[0-9]+)?' | sort -u)"
[ "$(printf '%s\n' "$UNIQ_VERS" | grep -c .)" -eq 1 ] \
  || die "BC suite is version-mismatched: $(printf '%s ' $UNIQ_VERS)" "$BCS"
[ "$(printf '%s\n1.85\n' "$UNIQ_VERS" | sort -V | head -1)" = "1.85" ] \
  || die "BC suite ${UNIQ_VERS} is < 1.85 (CVE-2026-8763)" "$BCS"

echo "==> the three scanner front-ends survived the bump and report versions"
# Probe each tool in its OWN container: a single `sh -c 'a; b; c'` returns only
# c's status, masking a failure of a or b. Require exit 0 AND a strict version
# line per tool (a bare "scanner" substring would also match an error/config
# line), so a broken front-end fails here.
check_tool() {
  local label="$1" bin="$2" flag="$3" pat="$4" out rc
  set +e
  out="$(docker run --rm --network none --entrypoint "$bin" "$IMG" "$flag" 2>&1)"; rc=$?
  set -e
  [ "$rc" -eq 0 ] || die "${label}: '${bin} ${flag}' exited ${rc}" "$out"
  grep -qE "$pat" <<<"$out" || die "${label}: version line /${pat}/ not found" "$out"
}
check_tool "gradle"        gradle        --version 'Gradle 8\.14\.6'
check_tool "sonar-scanner" sonar-scanner --version 'SonarScanner CLI 6\.2\.1\.4610'
check_tool "maven"         mvn           --version 'Apache Maven 3\.9'

echo "==> bundled gradle runs a real build that PGP-signs a file (exercises bcpg+bcprov)"
project_sign "$WOK"
run docker run --rm --name "$CNAME" --network none \
  -e GRADLE_USER_HOME=/workspace/.gradle-home -w /workspace -v "$WOK:/workspace" \
  --entrypoint gradle "$IMG" gocdnextSign --offline --no-daemon --console=plain
case "$RC" in
  124) die "signing: watchdog SIGTERM at 300s — gradle hung" "$OUT" ;;
  137) die "signing: watchdog SIGKILL after 300s+15s — gradle hung" "$OUT" ;;
esac
[ "$RC" -eq 0 ] || die "signing project: expected exit 0, got $RC" "$OUT"
grep -q 'GOCDNEXT_BC_SIGN_OK' <<<"$OUT" || die "signing project: sign task did not reach its OK marker" "$OUT"
grep -q 'BUILD SUCCESSFUL'    <<<"$OUT" || die "signing project: build did not succeed" "$OUT"
SIG="$(docker run --rm --network none --entrypoint sh -v "$WOK:/w" "$IMG" -c 'head -1 /w/payload.txt.asc 2>/dev/null')" || true
printf '%s\n' "$SIG" | grep -q 'BEGIN PGP SIGNATURE' || die "signing project: payload.txt.asc is not a PGP signature" "$SIG"

echo "==> entrypoint fails fast (exit 2) when no token is provided"
# Through the watchdog + named container too: a regression that hangs the
# entrypoint must fail the smoke (124/137), not wedge it past cleanup's reach.
run docker run --rm --name "$CNAME" --network none -e PLUGIN_PROJECT_KEY=smoke "$IMG"
case "$RC" in
  124) die "no-token: watchdog SIGTERM at 300s — entrypoint hung before the token check" "$OUT" ;;
  137) die "no-token: watchdog SIGKILL after 300s+15s — entrypoint hung" "$OUT" ;;
esac
[ "$RC" -eq 2 ] || die "no-token: expected exit 2, got $RC" "$OUT"
grep -q 'PLUGIN_TOKEN' <<<"$OUT" || die "no-token: did not explain the missing-token contract" "$OUT"

SMOKE_OK=1
echo "PASS: sonar smoke"
