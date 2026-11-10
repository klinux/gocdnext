#!/usr/bin/env bash
# Real smoke for the gradle plugin's IMAGE. Builds the pinned image and runs it
# end to end so the bouncycastle suite swap (CVE-2025-14813 + CVE-2026-8763)
# fails HERE, before publish. Proves:
#   - the bundled BC suite (bcpg/bcprov/bcutil) is a single version >= 1.85, no
#     1.78.1 jar survives — the exact jars Gradle loads for PGP/signing;
#   - the swapped crypto is actually FUNCTIONAL, not merely present: the plugin
#     runs a real `gradle` build that applies the core `signing` plugin and
#     PGP-signs a file with an in-memory key, exercising bcpg+bcprov. A broken
#     or API-incompatible jar would throw when the first BC class loads — a
#     println task would never catch that;
#   - a FAILING build exits 1 (no false green — proves the runner really ran).
#
# Fully offline (`--network none`): Gradle's `signing` is a core plugin (no
# download) and the key is generated locally per run, so nothing is committed.
# Built --no-cache to match a release build. Root-owned workspace outputs are
# cleaned in-image; fail-closed.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMG="gocdnext-plugin-gradle:smoke-$$"
CNAME="gradlesmoke-$$"   # fixed name so a timed-out run's container can be reaped
WOK="$(mktemp -d)"       # passing (signing) project
WBAD="$(mktemp -d)"      # failing project
WGLOB="$(mktemp -d)"     # #361 argv-probe project (glob non-expansion)
SMOKE_OK=0

cleanup() {
  # Kill any stuck run container FIRST: a timed-out one still holds $WOK/$WBAD
  # mounted, so the mount-cleaning container below would otherwise race a live
  # writer on the same dirs.
  docker rm -f "$CNAME" >/dev/null 2>&1 || true
  if docker image inspect "$IMG" >/dev/null 2>&1; then
    docker run --rm --entrypoint bash -v "$WOK:/a" -v "$WBAD:/b" -v "$WGLOB:/c" "$IMG" \
      -c 'for d in /a /b /c; do rm -rf "$d"/* "$d"/.[!.]* "$d"/..?* 2>/dev/null || true; done' \
      >/dev/null 2>&1 || true
  fi
  docker image rm -f "$IMG" >/dev/null 2>&1 || true
  rm -rf "$WOK" "$WBAD" "$WGLOB" 2>/dev/null || true
  leftover=0
  for d in "$WOK" "$WBAD" "$WGLOB"; do
    [ -d "$d" ] && { echo "WARN: cleanup could not fully remove ${d}" >&2; leftover=1; }
  done
  docker image inspect "$IMG" >/dev/null 2>&1 && { echo "WARN: smoke image not removed" >&2; leftover=1; }
  [ "$leftover" = 1 ] && [ "$SMOKE_OK" = 1 ] && { echo "FAIL: smoke passed but left state behind" >&2; exit 1; }
  return 0
}
trap cleanup EXIT
die() { echo "FAIL: $1"; [ -n "${2:-}" ] && { echo "--- output ---"; echo "$2" | tail -40; }; exit 1; }

command -v gpg >/dev/null 2>&1 || die "gpg is required to generate the disposable signing key"

# Disposable, unprotected PGP key, generated per run into an isolated GNUPGHOME —
# NEVER committed. The armored secret key lands in the passing project so the
# build can load it via useInMemoryPgpKeys.
gen_signing_key() {
  local gh; gh="$(mktemp -d)"
  GNUPGHOME="$gh" gpg --batch --gen-key >/dev/null 2>&1 <<'PARAMS'
%no-protection
Key-Type: RSA
Key-Length: 2048
Name-Real: gocdnext gradle smoke
Name-Email: smoke@example.invalid
Expire-Date: 0
%commit
PARAMS
  GNUPGHOME="$gh" gpg --batch --armor --export-secret-keys smoke@example.invalid 2>/dev/null
  rm -rf "$gh"
}

# A passing project: applies core `signing`, loads the in-memory PGP key, and
# signs payload.txt -> payload.txt.asc. This is the real bcpg+bcprov exercise.
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

# #361 argv probe: a stub `./gradlew` that prints the argv it was ACTUALLY
# exec'd with (the entrypoint prefers ./gradlew when executable). This is the
# only trustworthy witness of a glob — the entrypoint's own `==>` banner is
# double-quoted, so it prints `*` literally whether or not the real exec
# expanded it. Seed the dir with decoy files a `*` would match.
project_argv_probe() {
  local d="$1"
  cat >"$d/gradlew" <<'G'
#!/bin/sh
echo GRADLEW_ARGV_BEGIN
for a in "$@"; do printf 'ARG=%s\n' "$a"; done
echo GRADLEW_ARGV_END
G
  chmod +x "$d/gradlew"
  : >"$d/decoy-a.txt"; : >"$d/decoy-b.txt"
}
# Extract the stub's received argv (one arg per line) from the last run's OUT.
probe_argv() { awk '/GRADLEW_ARGV_BEGIN/{f=1;next} /GRADLEW_ARGV_END/{f=0} f'; }

# A failing project: a task that throws, to prove non-zero propagation.
project_fail() {
  local d="$1"
  cat >"$d/settings.gradle" <<'G'
rootProject.name = 'bc-smoke-fail'
G
  cat >"$d/build.gradle" <<'G'
tasks.register('gocdnextFail') {
  doLast { throw new GradleException('intentional smoke failure') }
}
G
}

# Hard wall-clock cap with TERM->KILL escalation so a hung run (ignored SIGTERM)
# is still bounded: TERM-stop surfaces as RC=124, a forced KILL as RC=137. The
# container is named so a timed-out, --rm-stranded one gets reaped in cleanup.
# 300s: a cold --no-daemon JVM + signing, offline.
run() { d="$1"; shift; docker rm -f "$CNAME" >/dev/null 2>&1 || true; set +e; OUT="$(timeout --kill-after=15s 300s docker run --rm --name "$CNAME" --network none -v "$d:/workspace" -w /workspace "$@" "$IMG" 2>&1)"; RC=$?; set -e; }

echo "==> build plugin image (--no-cache, same Dockerfile/context as the official build)"
docker build --no-cache -t "$IMG" "$HERE" >/dev/null

echo "==> bouncycastle suite is a single version >= 1.85, no 1.78.1 (CVE-2025-14813 + CVE-2026-8763 cleared)"
BCS="$(docker run --rm --network none --entrypoint sh "$IMG" -c 'ls -1 /opt/gradle/lib/plugins/ 2>/dev/null | grep -E "^bc(pg|prov|util)-jdk18on-[0-9]"')" \
  || die "could not list bouncycastle jars" "$BCS"
[ "$(printf '%s\n' "$BCS" | grep -c .)" -eq 3 ] || die "expected exactly 3 BC jars (bcpg/bcprov/bcutil)" "$BCS"
printf '%s\n' "$BCS" | grep -q -- '-1.78.1.jar' && die "a vulnerable BC 1.78.1 jar survived" "$BCS"
# BC ships bcpg/bcprov/bcutil together and they must match; a mixed suite (e.g. a
# stray older bcpg left behind) is unsupported and could reintroduce a CVE. Each
# jar name carries exactly one version, so collapse to the unique set and require
# EXACTLY ONE, then require that single version to clear the floor (>= 1.85).
UNIQ_VERS="$(printf '%s\n' "$BCS" | grep -oE '[0-9]+\.[0-9]+(\.[0-9]+)?' | sort -u)"
[ "$(printf '%s\n' "$UNIQ_VERS" | grep -c .)" -eq 1 ] \
  || die "BC suite is version-mismatched: $(printf '%s ' $UNIQ_VERS)" "$BCS"
[ "$(printf '%s\n1.85\n' "$UNIQ_VERS" | sort -V | head -1)" = "1.85" ] \
  || die "BC suite ${UNIQ_VERS} is < 1.85 (CVE-2026-8763)" "$BCS"

echo "==> plugin runs a real 'gradle' build that PGP-signs a file (exercises bcpg+bcprov)"
project_sign "$WOK"
run "$WOK" -e PLUGIN_COMMAND='gocdnextSign --offline'
[ "$RC" -eq 0 ] || die "signing project: expected exit 0, got $RC" "$OUT"
grep -q 'GOCDNEXT_BC_SIGN_OK' <<<"$OUT" || die "signing project: sign task did not reach its OK marker" "$OUT"
grep -q 'BUILD SUCCESSFUL' <<<"$OUT" || die "signing project: build did not succeed" "$OUT"
# Confirm the detached signature really exists in the workspace (read in-image;
# the file is root-owned).
SIG="$(docker run --rm --network none --entrypoint sh -v "$WOK:/w" "$IMG" -c 'head -1 /w/payload.txt.asc 2>/dev/null')" || true
printf '%s\n' "$SIG" | grep -q 'BEGIN PGP SIGNATURE' || die "signing project: payload.txt.asc is not a PGP signature" "$SIG"

echo "==> a FAILING build exits 1 within the cap (no false green, no hang)"
project_fail "$WBAD"
run "$WBAD" -e PLUGIN_COMMAND='gocdnextFail --offline'
case "$RC" in
  124) die "failing project: watchdog SIGTERM at 300s — gradle hung" "$OUT" ;;
  137) die "failing project: watchdog SIGKILL after 300s+15s (ignored SIGTERM) — gradle hung" "$OUT" ;;
esac
[ "$RC" -eq 1 ] || die "failing project: expected gradle's build-failure exit 1, got $RC" "$OUT"
grep -q 'BUILD FAILED' <<<"$OUT" || die "failing project: gradle did not report BUILD FAILED" "$OUT"

# ── #361: neither PLUGIN_COMMAND nor extra args glob-expand against the
#    workspace. Two UNQUOTED expansions in the exec (`${PLUGIN_COMMAND}` and
#    `${extra_args}`), so each is covered in its own run. The stub gradlew
#    reports the real argv; decoy-a.txt/decoy-b.txt would appear as separate
#    args if a `*` expanded. ──
project_argv_probe "$WGLOB"

echo "==> #361: a glob in PLUGIN_COMMAND reaches gradle literally (not workspace files)"
run "$WGLOB" -e PLUGIN_COMMAND='build *.txt'
[ "$RC" -eq 0 ] || die "argv probe (command): expected exit 0, got $RC" "$OUT"
AV="$(probe_argv <<<"$OUT")"
grep -qxF 'ARG=*.txt' <<<"$AV" || die "PLUGIN_COMMAND glob-expanded against the workspace (literal '*.txt' did not reach gradle)" "$AV"
grep -qxF 'ARG=decoy-a.txt' <<<"$AV" && die "PLUGIN_COMMAND glob-expanded (decoy filename leaked into argv)" "$AV"

echo "==> #361: a glob in PLUGIN_ARGS (extra args) reaches gradle literally"
run "$WGLOB" -e PLUGIN_COMMAND='build' -e PLUGIN_ARGS='*.txt'
[ "$RC" -eq 0 ] || die "argv probe (args): expected exit 0, got $RC" "$OUT"
AV="$(probe_argv <<<"$OUT")"
grep -qxF 'ARG=*.txt' <<<"$AV" || die "PLUGIN_ARGS glob-expanded against the workspace (literal '*.txt' did not reach gradle)" "$AV"
grep -qxF 'ARG=decoy-a.txt' <<<"$AV" && die "PLUGIN_ARGS glob-expanded (decoy filename leaked into argv)" "$AV"

SMOKE_OK=1
echo "PASS: gradle smoke"
