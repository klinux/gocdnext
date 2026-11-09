# gocdnext/gradle — Gradle wrapper runner. Prefers the
# project's `./gradlew` when present (the Gradle-best-practice
# path since the wrapper pins the exact version the project
# expects) and falls back to the bundled `gradle` CLI otherwise.
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_COMMAND      (required)  gradle tasks + args, word-split.
#                                   Examples:
#                                     "build"
#                                     "test --tests com.acme.AppTest"
#                                     "publish -Prelease=true"
#   PLUGIN_WORKING_DIR  (optional)  path under /workspace; defaults
#                                   to /workspace.
#   PLUGIN_GRADLE_OPTS  (optional)  extra JVM args for gradle
#                                   (exported as GRADLE_OPTS).
#                                   Useful for `-Xmx4g` on big builds.
#   PLUGIN_DAEMON       (optional)  "true"/"false"; default "false".
#                                   CI almost never benefits from the
#                                   daemon because each run is a fresh
#                                   container — kept as a knob for the
#                                   rare caching setup.
#   PLUGIN_JDK          (optional)  "11"|"17"|"21"|"25"; default "21".
#                                   Selects the JDK at runtime via
#                                   the shared jdk-base/select-jdk.sh
#                                   helper. Lets a single plugin tag
#                                   serve every supported toolchain
#                                   without per-JDK image variants.
#
# Ships Gradle 8.14.6 CLI as wrapper fallback. Real projects almost
# always ship `./gradlew` which pins the exact version they need;
# the CLI matters only on legacy projects without a wrapper.
ARG BASE_IMAGE=ghcr.io/klinux/gocdnext-plugin-jdk-base:v1
FROM ${BASE_IMAGE}

# Gradle CLI fallback. Pinned via build-arg + SHA256 so a Gradle
# bump is a two-line Dockerfile edit (version + sha) + rebuild.
# The sha is from https://services.gradle.org/distributions/
# gradle-${GRADLE_VERSION}-bin.zip.sha256 at release time —
# Gradle never republishes a tagged release.
# Gradle 8.14.6 (latest 8.x) bundles bouncycastle bcpg/bcprov/bcutil 1.85, which
# clears CVE-2025-14813 (bcprov < 1.80.2) and CVE-2026-8763 (bcprov < 1.85) that
# the old 8.10's bundled BC 1.78.1 carried. These jars are referenced by exact
# filename in Gradle's internal module registry, so swapping the jar in place
# breaks startup — a version bump is the only working fix. Staying on 8.x (not
# jumping to 9.x) avoids a major behaviour change for the legacy wrapperless
# projects this fallback CLI serves.
ARG GRADLE_VERSION=8.14.6
ARG GRADLE_SHA256=7988ed071b2a07900e2ec715fca15c6ab72bce6db433af301fa7fa7e9407bd24
RUN curl -fsSL "https://services.gradle.org/distributions/gradle-${GRADLE_VERSION}-bin.zip" -o /tmp/g.zip && \
    echo "${GRADLE_SHA256}  /tmp/g.zip" | sha256sum -c - && \
    unzip -q /tmp/g.zip -d /opt && \
    ln -s "/opt/gradle-${GRADLE_VERSION}" /opt/gradle && \
    rm /tmp/g.zip
ENV PATH=/opt/gradle/bin:$PATH

COPY entrypoint.sh /usr/local/bin/gocdnext-gradle
RUN chmod +x /usr/local/bin/gocdnext-gradle

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-gradle"]
