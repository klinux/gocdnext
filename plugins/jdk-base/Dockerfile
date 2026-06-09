# gocdnext-plugin-jdk-base — shared base image for every plugin
# that needs a JVM. Ships Temurin 11, 17, 21 (LTS) and 25 (LTS).
# Selected at runtime via `PLUGIN_JDK` (the operator's `jdk:`
# input on the plugin step) through select-jdk.sh, which any
# downstream entrypoint sources before launching its tool.
#
# Why one image instead of per-JDK tags:
#   - Layer cache at the K8s node: a node that runs both a
#     gradle job (Card, JDK 21) and a maven job (legacy, JDK 11)
#     pulls the JDKs ONCE because both plugin images share the
#     COPY --from=eclipse-temurin layers via this base.
#   - Operator UX: `uses: ...@v1` stays stable across version
#     bumps, the `jdk:` input is the only thing that varies per
#     pipeline. No tag-renaming dance on plugin upgrades.
#   - Convention: future JVM plugins (sbt, ant, kotlin-cli)
#     inherit this base and gain JDK switching for free.
#
# Stage 0-3: pull the official Temurin JDK images. The Adoptium
# Docker images install the JDK at /opt/java/openjdk; we move
# each to a versioned path in the final stage.
FROM eclipse-temurin:11-jdk AS jdk11
FROM eclipse-temurin:17-jdk AS jdk17
FROM eclipse-temurin:21-jdk AS jdk21
FROM eclipse-temurin:25-jdk AS jdk25

# Final stage: debian:bookworm-slim + the four JDKs. We pick
# bookworm-slim (not Alpine) so glibc-linked toolchains —
# Gradle's native services, Maven's resolver native bits — find
# their expected runtime. ~80 MB base. The four JDKs add ~800 MB
# combined; total image lands around ~880 MB.
FROM debian:bookworm-slim

# Tools every downstream plugin needs:
#   - ca-certificates: TLS to Maven Central, Nexus, plugin
#     portal, etc.
#   - curl, git: the bare minimum every JVM CI step touches.
#   - tar, gzip, unzip: artifact handling.
#   - locales: en_US.UTF-8 generated below so Kotlin compiler
#     warnings about default charset stay quiet across base
#     image bumps.
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl git tar gzip unzip locales \
    && rm -rf /var/lib/apt/lists/* \
    && sed -i '/en_US.UTF-8/s/^# //g' /etc/locale.gen && locale-gen

# Copy each JDK to a versioned path. The eclipse-temurin images
# install at /opt/java/openjdk; we relocate to /opt/java/jdk-N
# so the select-jdk.sh helper can address them by version
# without parsing release files.
COPY --from=jdk11 /opt/java/openjdk /opt/java/jdk-11
COPY --from=jdk17 /opt/java/openjdk /opt/java/jdk-17
COPY --from=jdk21 /opt/java/openjdk /opt/java/jdk-21
COPY --from=jdk25 /opt/java/openjdk /opt/java/jdk-25

# Default to JDK 21 (current most-deployed LTS). Downstream
# entrypoints source select-jdk.sh to override per-job via
# PLUGIN_JDK. Setting it here means a plugin that forgets to
# source the helper still has a working JAVA_HOME — failing
# closed to a working build, not to a broken one.
ENV JAVA_HOME=/opt/java/jdk-21
ENV PATH=/opt/java/jdk-21/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
ENV LANG=en_US.UTF-8
ENV LC_ALL=en_US.UTF-8

# The helper that downstream plugins source. Lives on PATH so
# `. select-jdk.sh` works regardless of CWD.
COPY select-jdk.sh /usr/local/bin/select-jdk.sh
RUN chmod +x /usr/local/bin/select-jdk.sh

# No ENTRYPOINT: this image is a base, not a runnable plugin.
# `docker run ghcr.io/.../jdk-base` falls through to /bin/sh.
