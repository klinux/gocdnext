# gocdnext/android — build + sign Android apps on the shared
# jdk-base image. A Bitrise-comparable Android *build* step: it
# runs Gradle (wrapper-first, same semantics as the gradle plugin)
# and signs the built artifacts (apksigner for .apk, jarsigner for
# .aab) when a keystore is provided via secrets.
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_COMMAND          (required)  gradle tasks + args, word-split.
#                                       e.g. "assembleRelease", "bundleRelease",
#                                       "testDebugUnitTest lintDebug".
#   PLUGIN_WORKING_DIR      (optional)  path under /workspace to cd into.
#   PLUGIN_JDK              (optional)  "11"|"17"|"21"|"25"; default "17"
#                                       (AGP 8.x floor). Selected at runtime via
#                                       the shared jdk-base select-jdk.sh helper.
#   PLUGIN_SDK_COMPONENTS   (optional)  extra sdkmanager packages, space-split.
#                                       e.g. "platforms;android-34 ndk;26.1.10909125".
#                                       The baked set covers the latest stable API;
#                                       extra components install per job (bake a
#                                       derived image to avoid re-downloading).
#   PLUGIN_GRADLE_OPTS      (optional)  extra JVM args (GRADLE_OPTS), e.g. "-Xmx6g".
#   PLUGIN_BUILD_CACHE      (optional)  tri-state --build-cache / --no-build-cache.
#   PLUGIN_PARALLEL         (optional)  tri-state --parallel / --no-parallel.
#   PLUGIN_CONFIGURATION_CACHE (optional) tri-state configuration cache.
#   PLUGIN_CONSOLE          (optional)  auto|plain|rich|verbose; default plain.
#   PLUGIN_DAEMON           (optional)  "true"/"false"; default "false".
#   PLUGIN_ARGS             (optional)  extra gradle args appended verbatim.
#   -- signing (all optional; signing runs only when PLUGIN_KEYSTORE is set) --
#   PLUGIN_KEYSTORE         (optional)  base64 of the keystore (from secrets:).
#   PLUGIN_KEYSTORE_PASSWORD(req. if signing) store password (from secrets:).
#   PLUGIN_KEY_ALIAS        (req. if signing) key alias.
#   PLUGIN_KEY_PASSWORD     (optional)  key password; defaults to the store one.
#   PLUGIN_SIGN_PATHS       (optional)  globs of artifacts to sign; default
#                                       auto-detect **/build/outputs/**/*.{apk,aab}.
#   PLUGIN_ZIPALIGN         (optional)  "true"/"false"; default true (APK only).
#
# Passwords never reach argv (apksigner `env:` / jarsigner `:env` refs) and the
# keystore is decoded to a 0600 tempfile outside the workspace, removed on exit.
ARG BASE_IMAGE=ghcr.io/klinux/gocdnext-plugin-jdk-base:v1
FROM ${BASE_IMAGE}

# Android SDK command-line tools. Pinned by build number + SHA256. Google
# publishes a sha1 in repository2-3.xml; that sha1 was verified on the pinned
# zip and the stronger sha256 is pinned here (same approach as the gradle
# plugin's Gradle dist pin). Bump = edit the two ARGs + rebuild.
#   cmdline-tools 12.0 (build 11076708) — the CLASSIC self-contained sdkmanager.
# NOTE: do NOT bump to cmdline-tools 20.0+ (build 14742923+) without re-testing:
# those ship the new "Android CLI" launcher that downloads an "embedded
# installation" on first run and breaks hermetic/offline CI (sdkmanager
# --list_installed fails with --network none). The classic tool installs any
# package (incl. android-36 / build-tools 36.0.0) fine — tool version does not
# gate package availability.
ARG ANDROID_CMDLINE_TOOLS_BUILD=11076708
ARG ANDROID_CMDLINE_TOOLS_SHA256=2d2d50857e4eb553af5a6dc3ad507a17adf43d115264b1afc116f95c92e5e258

# Baked default SDK set: latest STABLE platform (Android 16 = API 36) + matching
# build-tools (ships apksigner + zipalign). Any other API/build-tools a project
# needs comes via PLUGIN_SDK_COMPONENTS at runtime. Bump = edit + rebuild.
ARG ANDROID_API=36
ARG ANDROID_BUILD_TOOLS=36.0.0

ENV ANDROID_HOME=/opt/android-sdk
ENV ANDROID_SDK_ROOT=/opt/android-sdk

# cmdline-tools extract to `cmdline-tools/`; the SDK layout expects them under
# $ANDROID_HOME/cmdline-tools/latest/ so sdkmanager resolves sibling packages.
RUN set -eux; \
    mkdir -p "${ANDROID_HOME}/cmdline-tools"; \
    curl -fsSL "https://dl.google.com/android/repository/commandlinetools-linux-${ANDROID_CMDLINE_TOOLS_BUILD}_latest.zip" -o /tmp/ct.zip; \
    echo "${ANDROID_CMDLINE_TOOLS_SHA256}  /tmp/ct.zip" | sha256sum -c -; \
    unzip -q /tmp/ct.zip -d /tmp/ct; \
    mv /tmp/ct/cmdline-tools "${ANDROID_HOME}/cmdline-tools/latest"; \
    rm -rf /tmp/ct.zip /tmp/ct

ENV PATH=${ANDROID_HOME}/cmdline-tools/latest/bin:${ANDROID_HOME}/platform-tools:${ANDROID_HOME}/build-tools/${ANDROID_BUILD_TOOLS}:${PATH}

# Accept licenses once (sdkmanager records acceptance hashes under licenses/ so a
# runtime PLUGIN_SDK_COMPONENTS install of the same license family stays
# non-interactive) and install the baked set. Fail loud if the signing tools
# aren't where PATH expects them.
RUN set -eux; \
    yes | sdkmanager --licenses >/dev/null; \
    sdkmanager "platform-tools" "platforms;android-${ANDROID_API}" "build-tools;${ANDROID_BUILD_TOOLS}" >/dev/null; \
    command -v apksigner >/dev/null; \
    command -v zipalign >/dev/null; \
    command -v aapt2 >/dev/null

COPY entrypoint.sh /usr/local/bin/gocdnext-android
RUN chmod +x /usr/local/bin/gocdnext-android

WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/gocdnext-android"]
