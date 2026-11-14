#!/bin/bash
# gocdnext/android — build + sign Android apps on the shared
# jdk-base image. Runs Gradle (wrapper-first, same semantics as
# the gradle plugin) and, when a keystore is supplied, signs the
# built artifacts:
#   - .apk  → zipalign (opt) + apksigner  (v2/v3 scheme)
#   - .aab  → jarsigner (upload key; Play App Signing re-signs)
#
# The signing secret NEVER reaches argv: passwords are passed by
# `env:`/`:env` references, and the keystore is decoded from the
# PLUGIN_KEYSTORE env var into a 0600 tempfile OUTSIDE the
# workspace (so it is never captured by the platform's
# `artifacts:`/`cache:` blocks), removed on exit.
#
# See Dockerfile for the full PLUGIN_* contract.

set -euo pipefail

# --- JDK selection (testable seam) ----------------------------------------
# Android Gradle Plugin 8.x needs JDK 17+, so default to 17 (the AGP floor
# and the most common Android toolchain); operators override via `jdk:`.
# select-jdk.sh ships in jdk-base; SELECT_JDK lets entrypoint_test.sh stub it.
export PLUGIN_JDK="${PLUGIN_JDK:-17}"
SELECT_JDK="${SELECT_JDK:-/usr/local/bin/select-jdk.sh}"
# shellcheck source=/dev/null
. "$SELECT_JDK"

# parse_bool — strict bool normaliser; typos fail loud. Accepted:
# true|1|yes|on, false|0|no|off (case-insensitive). Empty = default.
parse_bool() {
    local name="$1" val="$2" default="$3"
    if [ -z "$val" ]; then printf '%s' "$default"; return 0; fi
    case "$(printf '%s' "$val" | tr '[:upper:]' '[:lower:]')" in
        true|1|yes|on)  printf 'true'  ;;
        false|0|no|off) printf 'false' ;;
        *)
            echo "gocdnext/android: $name accepts true|false|1|0|yes|no|on|off (got '$val')" >&2
            exit 2
            ;;
    esac
}

if [ -z "${PLUGIN_COMMAND:-}" ]; then
    echo "gocdnext/android: PLUGIN_COMMAND is required" >&2
    echo "  examples:" >&2
    echo "    command: assembleRelease" >&2
    echo "    command: bundleRelease" >&2
    echo "    command: testDebugUnitTest lintDebug" >&2
    exit 2
fi

if [ -n "${PLUGIN_WORKING_DIR:-}" ]; then
    cd "${PLUGIN_WORKING_DIR}"
fi

git config --global --add safe.directory '*' 2>/dev/null || true

# Keystore tempdir cleanup. Registered once here (not inside sign_artifacts)
# because an EXIT trap evaluates its body in the outer scope, where a function-
# local would be gone and trip `set -u`. sign_artifacts fills _KS_CLEANUP.
_KS_CLEANUP=""
_cleanup() { [ -n "${_KS_CLEANUP}" ] && rm -rf "${_KS_CLEANUP}"; return 0; }
trap _cleanup EXIT

# Keep Gradle's home inside the workspace so the platform `cache:` block can
# tar it (same rationale as the gradle plugin — $HOME/.gradle is outside the
# tarred workspace and would round-trip an empty blob).
export GRADLE_USER_HOME="${GRADLE_USER_HOME:-.gradle-home}"
mkdir -p "${GRADLE_USER_HOME}"

# --- extra SDK components (override of the baked default set) --------------
# The image bakes platform-tools + the latest stable platform/build-tools. A
# project on a different compileSdk declares exactly what it needs here; these
# install to $ANDROID_HOME per job (bake a derived image to avoid re-downloading).
if [ -n "${PLUGIN_SDK_COMPONENTS:-}" ]; then
    echo "==> installing extra SDK components: ${PLUGIN_SDK_COMPONENTS}"
    # `yes` auto-accepts any new component license and dies with SIGPIPE when
    # sdkmanager exits — guard set -e + pipefail around the pipe, then check
    # sdkmanager's own rc via PIPESTATUS.
    set +e +o pipefail
    # shellcheck disable=SC2086
    yes 2>/dev/null | sdkmanager ${PLUGIN_SDK_COMPONENTS}
    sdk_rc=${PIPESTATUS[1]}
    set -e -o pipefail
    if [ "${sdk_rc}" -ne 0 ]; then
        echo "gocdnext/android: sdkmanager failed installing '${PLUGIN_SDK_COMPONENTS}' (rc=${sdk_rc})" >&2
        exit 1
    fi
fi

# --- Gradle CLI: wrapper-first (pins the project's exact version) ----------
if [ -x "./gradlew" ]; then
    CLI="./gradlew"
elif command -v gradle >/dev/null 2>&1; then
    CLI="gradle"
else
    echo "gocdnext/android: neither ./gradlew nor gradle found" >&2
    exit 2
fi

# Daemon: default --no-daemon (ephemeral per-job pod never reuses it).
daemon_val=$(parse_bool daemon "${PLUGIN_DAEMON:-}" "false") || exit $?
if [ "$daemon_val" = "true" ]; then daemon_flag="--daemon"; else daemon_flag="--no-daemon"; fi

# Build cache — TRI-STATE. unset → no flag (respects gradle.properties);
# "true"/"false" force --build-cache / --no-build-cache.
build_cache_flag=""
if [ -n "${PLUGIN_BUILD_CACHE:-}" ]; then
    bc_val=$(parse_bool build-cache "${PLUGIN_BUILD_CACHE}" "") || exit $?
    if [ "$bc_val" = "true" ]; then build_cache_flag="--build-cache"; else build_cache_flag="--no-build-cache"; fi
fi

# Parallel — TRI-STATE, same shape.
parallel_flag=""
if [ -n "${PLUGIN_PARALLEL:-}" ]; then
    par_val=$(parse_bool parallel "${PLUGIN_PARALLEL}" "") || exit $?
    if [ "$par_val" = "true" ]; then parallel_flag="--parallel"; else parallel_flag="--no-parallel"; fi
fi

# Configuration cache — TRI-STATE, same shape.
config_cache_flag=""
if [ -n "${PLUGIN_CONFIGURATION_CACHE:-}" ]; then
    cc_val=$(parse_bool configuration-cache "${PLUGIN_CONFIGURATION_CACHE}" "") || exit $?
    if [ "$cc_val" = "true" ]; then config_cache_flag="--configuration-cache"; else config_cache_flag="--no-configuration-cache"; fi
fi

# Console mode — default plain (append-only log stream; rich/verbose leak
# cursor escapes). Typos fail loud.
console_val=$(printf '%s' "${PLUGIN_CONSOLE:-plain}" | tr '[:upper:]' '[:lower:]')
case "$console_val" in
    auto|plain|rich|verbose) console_flag="--console=${console_val}" ;;
    *)
        echo "gocdnext/android: console accepts auto|plain|rich|verbose (got '${PLUGIN_CONSOLE:-}')" >&2
        exit 2
        ;;
esac

if [ -n "${PLUGIN_GRADLE_OPTS:-}" ]; then
    export GRADLE_OPTS="${PLUGIN_GRADLE_OPTS}"
fi

extra_args="${PLUGIN_ARGS:-}"

# sign_artifacts — called after a successful build when PLUGIN_KEYSTORE is set.
sign_artifacts() {
    local alias="${PLUGIN_KEY_ALIAS:-}"
    if [ -z "${PLUGIN_KEYSTORE_PASSWORD:-}" ]; then
        echo "gocdnext/android: keystore-password is required when keystore is set" >&2
        exit 2
    fi
    if [ -z "$alias" ]; then
        echo "gocdnext/android: key-alias is required when keystore is set" >&2
        exit 2
    fi
    # Single-password keystores: key password defaults to the store password.
    export PLUGIN_KEY_PASSWORD="${PLUGIN_KEY_PASSWORD:-$PLUGIN_KEYSTORE_PASSWORD}"

    local ks_file
    _KS_CLEANUP="$(mktemp -d)"
    ks_file="${_KS_CLEANUP}/release.keystore"
    # Decode base64 → 0600 file outside the workspace (cleaned by the EXIT trap).
    # The keystore arrives via `with: keystore: ${{ SECRET }}` as the
    # PLUGIN_KEYSTORE env var, never argv.
    if ! ( umask 077; printf '%s' "${PLUGIN_KEYSTORE}" | base64 -d > "$ks_file" ) 2>/dev/null; then
        echo "gocdnext/android: keystore is not valid base64" >&2
        exit 2
    fi
    if [ ! -s "$ks_file" ]; then
        echo "gocdnext/android: decoded keystore is empty" >&2
        exit 2
    fi

    local zipalign_on
    zipalign_on=$(parse_bool zipalign "${PLUGIN_ZIPALIGN:-}" "true") || exit $?

    local -a targets=()
    shopt -s nullglob globstar
    if [ -n "${PLUGIN_SIGN_PATHS:-}" ]; then
        local p
        # shellcheck disable=SC2206
        for p in ${PLUGIN_SIGN_PATHS}; do targets+=( $p ); done
    else
        targets+=( **/build/outputs/**/*.apk **/build/outputs/**/*.aab )
    fi
    shopt -u nullglob globstar

    if [ "${#targets[@]}" -eq 0 ]; then
        echo "gocdnext/android: signing requested but no .apk/.aab found under **/build/outputs/** — set sign-paths: to point at them" >&2
        exit 1
    fi

    local f signed=0
    for f in "${targets[@]}"; do
        [ -f "$f" ] || continue
        case "$f" in
            *.apk)
                if [ "$zipalign_on" = "true" ]; then
                    echo "==> zipalign ${f}"
                    zipalign -f -p 4 "$f" "${f}.aligned"
                    mv "${f}.aligned" "$f"
                fi
                echo "==> apksigner sign ${f}"
                apksigner sign \
                    --ks "$ks_file" \
                    --ks-key-alias "$alias" \
                    --ks-pass env:PLUGIN_KEYSTORE_PASSWORD \
                    --key-pass env:PLUGIN_KEY_PASSWORD \
                    "$f"
                apksigner verify --verbose "$f" >/dev/null
                echo "    signed + verified: ${f}"
                signed=$((signed + 1))
                ;;
            *.aab)
                # apksigner cannot sign AABs — the bundle is signed with the
                # upload key via jarsigner; Play App Signing re-signs it for
                # distribution.
                echo "==> jarsigner ${f}"
                jarsigner \
                    -keystore "$ks_file" \
                    -storepass:env PLUGIN_KEYSTORE_PASSWORD \
                    -keypass:env PLUGIN_KEY_PASSWORD \
                    -sigalg SHA256withRSA -digestalg SHA-256 \
                    "$f" "$alias" >/dev/null
                echo "    signed: ${f}"
                signed=$((signed + 1))
                ;;
            *)
                echo "gocdnext/android: don't know how to sign '${f}' (expected .apk/.aab)" >&2
                exit 1
                ;;
        esac
    done

    # Fail loud if signing was requested but nothing was actually signed — e.g.
    # an explicit sign-paths: pointing at a file that doesn't exist (a literal,
    # non-glob path isn't dropped by nullglob, so it survives into targets but
    # the -f test skips it). A silent success here would ship UNSIGNED artifacts.
    if [ "$signed" -eq 0 ]; then
        echo "gocdnext/android: signing requested but no artifact was signed — sign-paths matched no existing .apk/.aab (${PLUGIN_SIGN_PATHS:-auto-detect})" >&2
        exit 1
    fi
}

# Unquoted expansion is intentional: empty tri-state flags disappear via
# word-splitting, and PLUGIN_COMMAND/extra_args are word-split into argv.
# `set -f` disables pathname (glob) expansion so a `*`/`?` in the command or
# args reaches gradle literally instead of matching workspace files (#361).
set -f
echo "==> ${CLI} ${daemon_flag} ${console_flag} ${build_cache_flag} ${parallel_flag} ${config_cache_flag} ${PLUGIN_COMMAND} ${extra_args}"
set +e
# shellcheck disable=SC2086
"${CLI}" \
    ${daemon_flag} \
    ${console_flag} \
    ${build_cache_flag} \
    ${parallel_flag} \
    ${config_cache_flag} \
    ${PLUGIN_COMMAND} \
    ${extra_args}
build_rc=$?
set -e
set +f

if [ "$build_rc" -ne 0 ]; then
    exit "$build_rc"
fi

if [ -n "${PLUGIN_KEYSTORE:-}" ]; then
    sign_artifacts
fi

echo "gocdnext/android: done"
