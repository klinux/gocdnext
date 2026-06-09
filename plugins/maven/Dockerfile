# gocdnext/maven — Maven wrapper with Nexus/Artifactory auth
# support. Covers the standard Java CI shape: test → package →
# deploy against a private repo manager.
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_COMMAND       (required)  mvn subcommand + args, word-split.
#                                    Examples:
#                                      "verify"
#                                      "clean package -DskipTests"
#                                      "deploy -Pprod"
#   PLUGIN_WORKING_DIR   (optional)  path under /workspace; cd'd
#                                    before running mvn. Defaults to
#                                    /workspace (root with pom.xml).
#   PLUGIN_SETTINGS      (optional)  path to settings.xml relative to
#                                    the workspace. When empty, the
#                                    entrypoint synthesises one from
#                                    NEXUS_* env vars (see below)
#                                    if any are set.
#   PLUGIN_NEXUS_URL     (optional)  base URL of the repository
#                                    manager (e.g.
#                                    "https://nexus.example.com/repository").
#   PLUGIN_NEXUS_USERNAME / PLUGIN_NEXUS_PASSWORD
#                        (optional)  auth for the synthesised
#                                    settings.xml. Pipe from
#                                    `secrets:` so plaintext stays
#                                    out of the log.
#   PLUGIN_SNAPSHOT_REPO_ID / PLUGIN_RELEASE_REPO_ID
#                        (optional)  server IDs embedded in
#                                    distributionManagement entries
#                                    of the pom. Default "snapshots"
#                                    and "releases".
#   PLUGIN_JDK           (optional)  "11"|"17"|"21"|"25"; default "21".
#                                    Selects the JDK at runtime via
#                                    the shared jdk-base/select-jdk.sh
#                                    helper. Same convention as the
#                                    gradle plugin — one tag covers
#                                    every supported toolchain.
#
# Ships Maven 3.9.9 + the four LTS JDKs (11, 17, 21, 25) via the
# shared jdk-base image. Pre-v0.15 the plugin was pinned to JDK
# 21 only; the v0.15 refactor closed that gap so legacy apps
# don't need a separate plugin variant.
ARG BASE_IMAGE=ghcr.io/klinux/gocdnext-plugin-jdk-base:v1
FROM ${BASE_IMAGE}

# Maven CLI. Pinned via build-arg + SHA512 so the version bump
# is a two-line Dockerfile edit (version + sha) + rebuild. We
# pull from archive.apache.org (NOT dlcdn) because dlcdn is a
# rolling mirror that drops older 3.9.x releases as new ones
# land (in 2026-06 dlcdn only carries 3.9.16); archive keeps
# every cut tarball forever. The sha is from
# https://archive.apache.org/dist/maven/maven-3/${MAVEN_VERSION}/
# binaries/apache-maven-${MAVEN_VERSION}-bin.tar.gz.sha512 —
# Apache never republishes a tagged release.
ARG MAVEN_VERSION=3.9.9
ARG MAVEN_SHA512=a555254d6b53d267965a3404ecb14e53c3827c09c3b94b5678835887ab404556bfaf78dcfe03ba76fa2508649dca8531c74bca4d5846513522404d48e8c4ac8b
RUN curl -fsSL "https://archive.apache.org/dist/maven/maven-3/${MAVEN_VERSION}/binaries/apache-maven-${MAVEN_VERSION}-bin.tar.gz" -o /tmp/m.tar.gz && \
    echo "${MAVEN_SHA512}  /tmp/m.tar.gz" | sha512sum -c - && \
    tar -xzf /tmp/m.tar.gz -C /opt && \
    ln -s "/opt/apache-maven-${MAVEN_VERSION}" /opt/maven && \
    rm /tmp/m.tar.gz
ENV MAVEN_HOME=/opt/maven
ENV PATH=/opt/maven/bin:$PATH

COPY entrypoint.sh /usr/local/bin/gocdnext-maven
RUN chmod +x /usr/local/bin/gocdnext-maven

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-maven"]
