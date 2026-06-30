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
    uses: ghcr.io/klinux/gocdnext-plugin-gradle@v1
    with:
      command: check --no-daemon --build-cache
    cache:
      - key: gradle-${CI_COMMIT_BRANCH}
        paths:
          - .gradle-user-home
          - .gradle-cache
    test_reports:
      - "**/build/test-results/test/*.xml"
    artifacts:
      optional:
        - "**/build/reports/jacoco/test/jacocoTestReport.xml"
        - "**/build/reports/tests/test/index.html"

  assemble:
    stage: build
    uses: ghcr.io/klinux/gocdnext-plugin-gradle@v1
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
  uses: ghcr.io/klinux/gocdnext-plugin-gradle@v1
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
  uses: ghcr.io/klinux/gocdnext-plugin-gradle@v1
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

## Dependency scanning (SCA)

The [`osv-scanner`](/gocdnext/docs/reference/plugins/#osv-scanner) plugin reads a
**resolved dependency list** — a lockfile or an SBOM — not source. Gradle doesn't
produce one by default: `build.gradle(.kts)` is a build script, not a resolved
graph. So a plain osv-scanner job over a Gradle repo reports *"No package sources
found"* and scans **nothing** (a misleadingly green result). Give it a resolved
list; either form below is auto-detected by `osv-scanner scan source --recursive .`
(what the plugin runs) — no extra flag.

### Option A — CycloneDX SBOM (recommended)

Apply the [cyclonedx-gradle-plugin](https://github.com/CycloneDX/cyclonedx-gradle-plugin)
and run its task in the build job; the SBOM captures the full resolved graph
(transitives + purls) and also works with trivy, grype, and Dependency-Track.

```kotlin title="build.gradle.kts"
plugins {
  id("org.cyclonedx.bom") version "1.10.0"
}
```

```yaml title=".gocdnext/security.yaml"
stages: [build, scan]

jobs:
  sbom:
    stage: build
    uses: ghcr.io/klinux/gocdnext-plugin-gradle@v1
    with:
      command: cyclonedxBom --no-daemon
    cache:
      - key: gradle-${CI_COMMIT_BRANCH}
        paths: [.gradle-user-home, .gradle-cache]
    artifacts:
      paths: ["build/reports/bom.json"]   # root task aggregates subprojects

  sca:
    stage: scan
    uses: ghcr.io/klinux/gocdnext-plugin-osv-scanner@v1
    needs_artifacts:
      - from_job: sbom
        paths: ["build/reports/bom.json"]
        dest: ./
    artifacts:
      optional: [osv-report.sarif]        # → Security dashboard
```

The `sca` job restores the SBOM with `needs_artifacts:`, osv-scanner auto-detects
`build/reports/bom.json` in the dir scan, and the SARIF flows into the
[Security dashboard](/gocdnext/docs/concepts/security/).

**Don't want to touch `build.gradle`?** Apply the plugin at invocation via an
init script committed alongside the pipeline — the repo's build files stay clean:

```groovy title="gradle/cyclonedx.init.gradle"
initscript {
  repositories { mavenCentral() }
  dependencies { classpath("org.cyclonedx:cyclonedx-gradle-plugin:1.10.0") }
}
allprojects { apply plugin: org.cyclonedx.gradle.CycloneDxPlugin }
```

```yaml
  with:
    command: --init-script gradle/cyclonedx.init.gradle cyclonedxBom --no-daemon
```

### Option B — native `gradle.lockfile`

Enable [Gradle dependency locking](https://docs.gradle.org/current/userguide/dependency_locking.html)
and commit the lockfile; osv-scanner reads `gradle.lockfile` directly.

```kotlin title="build.gradle.kts"
dependencyLocking { lockAllConfigurations() }
```

```bash
./gradlew dependencies --write-locks   # writes gradle.lockfile — commit it
```

The SBOM is richer (full transitive graph, portable across tools); the native
lockfile is simpler if you already use dependency locking.

### Why the scanner doesn't generate it

Producing either artifact **runs Gradle** — it resolves dependencies, executes
the build configuration, and needs the JDK, the wrapper, and your repositories.
That belongs in the build job, which already has the toolchain — not in the
osv-scanner container, which is a tiny read-only scanner. Running the build from
a scan step would also be arbitrary code execution in a step that should only
read. So the pattern is **build job emits → scan job consumes**.

Until you wire one of these up, set `fail-on-no-sources: "true"` on the
osv-scanner job so "Gradle without a lockfile or SBOM → nothing scanned" fails
**loud** instead of reporting a clean pass that scanned no dependencies.

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
