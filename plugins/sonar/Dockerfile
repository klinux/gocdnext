# gocdnext/sonar — single-image analyzer for SonarQube (self-
# hosted) and SonarCloud (SaaS). Bundles three scanner front-ends:
#
#   1. Maven           (`mvn sonar:sonar`)
#   2. Gradle          (`gradle sonar` — needs `org.sonarqube` plugin
#                       applied in build.gradle; project doesn't
#                       have to vendor the scanner binary)
#   3. Scanner CLI     (`sonar-scanner` — non-JVM languages: JS/TS,
#                       Python, Go, C/C++, etc.)
#
# Mode is auto-detected at runtime from the presence of pom.xml /
# build.gradle{,.kts} / neither, or overridden via `mode:` input.
# Trade-off: ~600 MB image vs three smaller specialised images —
# we picked one because operators don't want to think about which
# variant their `uses:` line points at, and the scan workload
# itself is JVM-bound either way.
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_TOKEN              (optional) Sonar auth token. Exported as
#                                        SONAR_TOKEN so the scanner
#                                        picks it up via env without
#                                        landing on a CLI argv
#                                        (no `ps auxww` leak).
#                                        Empty value falls back to
#                                        the `SONAR_TOKEN` env if set
#                                        — operators using
#                                        `secrets: [SONAR_TOKEN]` on
#                                        the job can skip the input
#                                        entirely. Runtime fails fast
#                                        when NEITHER is set.
#   PLUGIN_HOST_URL           (optional) Default https://sonarcloud.io;
#                                        set to your SonarQube base
#                                        URL for self-hosted.
#   PLUGIN_ORGANIZATION       (cloud)    Required for SonarCloud
#                                        (sonar.organization);
#                                        ignored on SonarQube.
#   PLUGIN_PROJECT_KEY        (required) sonar.projectKey.
#   PLUGIN_PROJECT_NAME       (optional) sonar.projectName; defaults
#                                        to projectKey.
#   PLUGIN_PROJECT_VERSION    (optional) sonar.projectVersion;
#                                        defaults to CI_COMMIT_SHORT_SHA
#                                        (predictable audit trail).
#   PLUGIN_MODE               (optional) auto | maven | gradle |
#                                        scanner-cli. Default auto.
#   PLUGIN_WORKING_DIR        (optional) path under /workspace.
#   PLUGIN_SOURCES            (optional) sonar.sources path(s).
#   PLUGIN_TESTS              (optional) sonar.tests path(s).
#   PLUGIN_BRANCH             (optional) sonar.branch.name. Skip on
#                                        SonarQube Community
#                                        Edition (branch analysis
#                                        is a paid feature).
#   PLUGIN_PULL_REQUEST_KEY   (optional) PR/MR id. When set, the
#                                        scanner switches to PR
#                                        decoration mode.
#   PLUGIN_PULL_REQUEST_BRANCH(optional) source branch.
#   PLUGIN_PULL_REQUEST_BASE  (optional) target branch (e.g. main).
#   PLUGIN_WAIT_FOR_QUALITY_GATE
#                             (optional) "true" → -Dsonar.qualitygate.wait=true;
#                                        scanner blocks until the
#                                        server evaluates the gate
#                                        and exits non-zero on
#                                        gate-fail. Default
#                                        "false" (scan-and-go).
#   PLUGIN_QUALITY_GATE_TIMEOUT
#                             (optional) seconds to wait for the
#                                        gate when wait is on.
#                                        Default 300.
#   PLUGIN_EXTRA_PROPS        (optional) Newline-separated
#                                        `-Dsonar.x=y` pairs, ONE
#                                        per line. Values may
#                                        contain whitespace.
#                                        Auth-bearing props
#                                        (sonar.token, sonar.login,
#                                        sonar.password) are
#                                        rejected at runtime.
#                                        Use for niche properties
#                                        (coverage report paths,
#                                        exclusions, …).
#
# Cache layout: the entrypoint exports SONAR_USER_HOME to a
# workspace-relative dir (.sonar-cache by default) so the
# platform's `cache:` block can tar the analyzer download cache
# across runs. First scan: ~80MB downloaded; subsequent runs hit
# the cache.
FROM maven:3.9-eclipse-temurin-21

# --- system deps -------------------------------------------------
# apt-get upgrade refreshes OS packages fixed since the base digest was cut
# (OpenSSL/libssl3, apt/libapt, libbrotli HIGHs) — the base is Ubuntu-based and
# mutable, so the plugin opts into no_cache:true to avoid re-importing a stale
# apt layer on a release that doesn't touch this Dockerfile.
RUN apt-get update && apt-get upgrade -y && apt-get install -y --no-install-recommends \
        ca-certificates git curl unzip jq \
    && rm -rf /var/lib/apt/lists/*

# --- Sonar Scanner CLI ------------------------------------------
# Version pinned to the latest stable; SHA256 default is the
# official checksum from SonarSource's distribution server
# (https://binaries.sonarsource.com/.../sonar-scanner-cli-
# ${SONAR_SCANNER_VERSION}-linux-x64.zip.sha256). Defaults
# baked here so every published image — and every contributor's
# local build — verifies the same byte sequence. Operators
# bumping VERSION must also pass the matching SHA256 via
# `--build-arg SONAR_SCANNER_SHA256=...`, otherwise the build
# fails loud at `sha256sum -c -`.
ARG SONAR_SCANNER_VERSION=6.2.1.4610
ARG SONAR_SCANNER_SHA256=0b8a3049f0bd5de7abc1582c78c233960d3d4ed7cc983a1d1635e8552f8bb439
RUN curl -fsSL -o /tmp/scanner.zip \
        "https://binaries.sonarsource.com/Distribution/sonar-scanner-cli/sonar-scanner-cli-${SONAR_SCANNER_VERSION}-linux-x64.zip" \
    && echo "${SONAR_SCANNER_SHA256}  /tmp/scanner.zip" | sha256sum -c - \
    && unzip -q /tmp/scanner.zip -d /opt \
    && mv "/opt/sonar-scanner-${SONAR_SCANNER_VERSION}-linux-x64" /opt/sonar-scanner \
    && rm /tmp/scanner.zip
ENV PATH="/opt/sonar-scanner/bin:${PATH}"

# --- Gradle 8.14.6 (matches the gocdnext/gradle plugin) ---------
# Same pin pattern as scanner-cli — default SHA256 is the
# canonical checksum from
# services.gradle.org/distributions/gradle-${VERSION}-bin.zip.sha256.
# Bumping VERSION requires bumping SHA256.
#
# 8.10 bundled bouncycastle bcpg/bcprov/bcutil 1.78.1, whose bcprov carried
# CVE-2025-14813 and CVE-2026-8763 (both CRITICAL). Gradle references these jars
# by exact filename in its internal module registry, so swapping the jar breaks
# startup — the fix is a version bump. 8.14.6 (latest 8.x) bundles the BC suite
# at 1.85, clearing both; staying on 8.x avoids the 9.x major behaviour change.
ARG GRADLE_VERSION=8.14.6
ARG GRADLE_SHA256=7988ed071b2a07900e2ec715fca15c6ab72bce6db433af301fa7fa7e9407bd24
RUN curl -fsSL -o /tmp/gradle.zip \
        "https://services.gradle.org/distributions/gradle-${GRADLE_VERSION}-bin.zip" \
    && echo "${GRADLE_SHA256}  /tmp/gradle.zip" | sha256sum -c - \
    && unzip -q /tmp/gradle.zip -d /opt \
    && mv "/opt/gradle-${GRADLE_VERSION}" /opt/gradle \
    && rm /tmp/gradle.zip
ENV PATH="/opt/gradle/bin:${PATH}"

# --- analyzer cache lives in the workspace ----------------------
# SONAR_USER_HOME is what the scanner reads for its analyzer
# download cache. Default points at the workspace so the
# platform's `cache:` block captures it.
ENV SONAR_USER_HOME=/workspace/.sonar-cache

# --- entrypoint -------------------------------------------------
COPY entrypoint.sh /usr/local/bin/gocdnext-sonar
RUN chmod +x /usr/local/bin/gocdnext-sonar

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-sonar"]
