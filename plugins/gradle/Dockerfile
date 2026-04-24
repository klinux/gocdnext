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
#
# Ships with Gradle 8.10 + JDK 21. Most pipelines will hit
# ./gradlew anyway, so the bundled version only matters on
# projects without a wrapper.
FROM gradle:8.10-jdk21

RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates git \
    && rm -rf /var/lib/apt/lists/*

COPY entrypoint.sh /usr/local/bin/gocdnext-gradle
RUN chmod +x /usr/local/bin/gocdnext-gradle

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-gradle"]
