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
#
# Ships with Maven 3.9 + JDK 21. Operators who need an older JDK
# can override via `image:` on the step since any Maven-bearing
# image honours the same PLUGIN_* contract.
FROM maven:3.9-eclipse-temurin-21

RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates git \
    && rm -rf /var/lib/apt/lists/*

COPY entrypoint.sh /usr/local/bin/gocdnext-maven
RUN chmod +x /usr/local/bin/gocdnext-maven

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-maven"]
