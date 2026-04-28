---
title: Gradle (Android, Kotlin, Java)
description: A Gradle project — assemble, run tests, attach reports, with both the Gradle user home AND the Gradle daemon caches preserved across runs.
---

The [`gocdnext/gradle`](/gocdnext/docs/reference/plugins/#gradle)
plugin wraps a JDK + Gradle wrapper image. Tests run, the artefact
is assembled, JUnit XML lands in the Tests tab, the build cache +
configuration cache + dependency cache all survive across runs so
warm builds drop to seconds.

## Layout assumed

```
repo/
├── settings.gradle.kts (or .gradle)
├── build.gradle.kts
├── gradlew                   # wrapper script
├── gradle/wrapper/...
├── app/                      # one or more subprojects
│   ├── build.gradle.kts
│   └── src/...
└── build/                    # generated
```

The plugin always uses `./gradlew` (the wrapper) so the Gradle
version is locked by the repo, not the agent's image — pin the
wrapper, pin the build.

## The pipeline

```yaml title=".gocdnext/ci.yaml"
name: ci

when:
  event: [push, pull_request]

stages: [test, build]

jobs:
  check:
    stage: test
    uses: gocdnext/gradle@v1
    with:
      command: check --no-daemon --build-cache
    cache:
      - key: gradle-${CI_COMMIT_BRANCH}
        paths:
          - .gradle-user-home
          - .gradle-cache
    test_reports:
      paths: ["**/build/test-results/test/*.xml"]
    artifacts:
      optional:
        paths:
          - "**/build/reports/jacoco/test/jacocoTestReport.xml"
          - "**/build/reports/tests/test/index.html"

  assemble:
    stage: build
    uses: gocdnext/gradle@v1
    needs: [check]
    with:
      command: assemble --no-daemon --build-cache -x test
    cache:
      - key: gradle-${CI_COMMIT_BRANCH}
        paths:
          - .gradle-user-home
          - .gradle-cache
    artifacts:
      paths:
        - "app/build/libs/*.jar"
        - "**/build/distributions/*"
```

What's worth highlighting:

### `--no-daemon` is mandatory in CI

The Gradle daemon was designed for IDE workflows where the JVM
stays warm across invocations. In CI, where each job runs in a
fresh container that's destroyed at the end, the daemon does the
opposite of what you want: it spawns, hangs around, holds locks
on the cache, and never serves a second request. `--no-daemon`
keeps each invocation single-shot.

### Two cache paths

`gradle/` plugin redirects two locations into the workspace so the
platform's `cache:` block can tar both:
- `.gradle-user-home` — the dependency cache (`GRADLE_USER_HOME`).
  Equivalent to `~/.gradle/`. Big — typically 300-500 MB.
- `.gradle-cache` — the build cache (`--build-cache` output). The
  task-output memo: if your inputs haven't changed, Gradle pulls
  the output bytes from here instead of re-running the task.

Warm builds with both populated hit the cache for compilation,
test fixtures, resource processing — a 3-minute cold build often
drops to under 30 seconds.

### `--build-cache`

Off by default in older Gradle versions, on by default in 8.x.
The flag is harmless to leave in either way and turns off-by-
default surprises into a no-op.

### `-x test` on assemble

`assemble` re-runs `test` by default. We already ran it in the
`check` job, and the artefacts cache means the compiled classes
are reused. Skipping the duplicate test run cuts the assemble
job by half.

## Variations

### Android app — release build with bundle

```yaml
bundle:
  stage: build
  uses: gocdnext/gradle@v1
  needs: [check]
  with:
    command: bundleRelease --no-daemon --build-cache
  cache:
    - key: gradle-${CI_COMMIT_BRANCH}
      paths: [.gradle-user-home, .gradle-cache]
  secrets:
    - ANDROID_KEYSTORE_BASE64
    - ANDROID_KEY_ALIAS
    - ANDROID_KEY_PASSWORD
  artifacts:
    paths:
      - "app/build/outputs/bundle/release/*.aab"
      - "app/build/outputs/mapping/release/mapping.txt"
```

Decode the keystore in `app/build.gradle.kts` from the env var, or
use the `signingConfig` block — gocdnext's secrets layer masks the
base64 string in logs.

### Multi-module, parallel

```yaml
check:
  stage: test
  uses: gocdnext/gradle@v1
  with:
    command: check --no-daemon --build-cache --parallel
  ...
```

`--parallel` lets independent subprojects run in parallel. Most
modern Gradle multi-module builds are configured for this; the
flag's a no-op when project dependencies serialise the work.

### Convention plugins / build-logic

If you use the `build-logic/` convention pattern, the build cache
benefits compound — convention compilation is a major chunk of cold
builds, and warm runs skip it entirely.

## Common pitfalls

- **`org.gradle.parallel=true` in `gradle.properties`** is fine,
  but combine it with `org.gradle.workers.max=N` to bound the JVM
  count under CI memory limits. Default = `Runtime.availableProcessors`
  which on a 4-vCPU agent with 8 GiB RAM thrashes.
- **`org.gradle.daemon=true`** in `gradle.properties` is silently
  overridden by `--no-daemon` — you don't need to remove it, just
  know which wins.
- **Configuration cache** (`--configuration-cache`) is great but
  finicky on legacy plugins. Adopt incrementally; if you hit a
  cache-incompatible plugin, remove the flag rather than disabling
  the cache for the whole build.
