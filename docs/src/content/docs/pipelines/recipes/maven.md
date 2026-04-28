---
title: Maven (Java/Kotlin)
description: A Maven project — test, package, attach JAR + reports, with the local repository cached across runs.
---

The [`gocdnext/maven`](/gocdnext/docs/reference/plugins/#maven) plugin
runs `mvn` in a container that has the JDK + Maven preinstalled. This
recipe covers the 90% case: test, package a deployable JAR, attach
the JUnit XML for the dashboard's Tests tab, ship the JAR + reports
as artefacts. Cache lives in `.m2/` so a warm run never re-downloads
the dependency tree.

## Layout assumed

```
repo/
├── pom.xml
├── src/
│   ├── main/java/...
│   └── test/java/...
└── target/                # generated
```

Multi-module reactor (parent POM + child modules) works the same
way — `mvn` walks the reactor automatically; the pipeline doesn't
need to know.

## The pipeline

```yaml title=".gocdnext/ci.yaml"
name: ci

when:
  event: [push, pull_request]

stages: [test, package]

jobs:
  test:
    stage: test
    uses: gocdnext/maven@v1
    with:
      command: -B -ntp test
    cache:
      - key: maven-${CI_COMMIT_BRANCH}
        paths: [.m2]
    test_reports:
      paths: ["target/surefire-reports/*.xml"]
    artifacts:
      optional:
        paths: ["target/site/jacoco/jacoco.xml"]

  package:
    stage: package
    uses: gocdnext/maven@v1
    needs: [test]
    with:
      # -DskipTests because tests already ran in the test stage and
      # we trust their result. -DfinalName stamps the JAR name so
      # the artefact path is deterministic regardless of the version
      # in pom.xml.
      command: -B -ntp -DskipTests -DfinalName=app package
    cache:
      - key: maven-${CI_COMMIT_BRANCH}
        paths: [.m2]
    artifacts:
      paths: ["target/app.jar"]
```

What's worth highlighting:

### `-B -ntp` is mandatory in CI

`-B` (batch mode) silences interactive prompts and ANSI noise.
`-ntp` (`--no-transfer-progress`) drops the per-dependency download
progress that turns the log into a wall of `Downloading … (12 KB
of 25 MB at …)` lines. Without these, a cold run dumps tens of
thousands of lines into `log_lines` for nothing.

### `cache: .m2`

The plugin's entrypoint redirects `MAVEN_USER_HOME` to
`/workspace/.m2` so the platform's `cache:` block can tar it.
Default is `~/.m2/repository` which the agent can't see. Warm
runs drop a typical 200-dependency project from minutes to
seconds.

Key `maven-${CI_COMMIT_BRANCH}` keeps main + feature branches
isolated — protects you from a poisoned cache leaking from a PR
into trunk builds.

### `test_reports:`

Surefire's XML reports are what populates the **Tests** tab in
the run detail page (per-job pass/fail/skip counts, click into
failures with the exception message). Pattern is glob — adjust
when you also want Failsafe (`target/failsafe-reports/*.xml`).

### `optional:` for coverage

Some jobs don't generate a JaCoCo report (no `-Pcoverage` profile,
project pre-instrumentation), and you don't want CI to fail because
of a missing artefact in that case. `optional:` is "publish if
present, no-op if missing".

## Variations

### With JaCoCo + Codecov

```yaml
test:
  stage: test
  uses: gocdnext/maven@v1
  with:
    command: -B -ntp -Pcoverage verify
  cache:
    - key: maven-${CI_COMMIT_BRANCH}
      paths: [.m2]
  test_reports:
    paths: ["target/**/surefire-reports/*.xml"]
  artifacts:
    paths: ["target/site/jacoco/jacoco.xml"]

upload-coverage:
  stage: test
  uses: gocdnext/codecov@v1
  needs: [test]
  needs_artifacts:
    - from_job: test
      paths: ["target/site/jacoco/jacoco.xml"]
  with:
    file: target/site/jacoco/jacoco.xml
    flags: maven
  secrets:
    - CODECOV_TOKEN
```

### Multi-module reactor with parallel build

```yaml
test:
  stage: test
  uses: gocdnext/maven@v1
  with:
    command: -B -ntp -T 1C test
  ...
```

`-T 1C` runs one thread per CPU core. On a 4-core agent this
roughly halves a 4-module reactor's build time. Don't push beyond
`1C` unless tests are deterministic under concurrency — Surefire
forks JVMs per module, which most of the time is fine.

### Push artefact to a private Nexus

```yaml
deploy:
  stage: package
  uses: gocdnext/maven@v1
  needs: [package]
  with:
    command: -B -ntp -DskipTests -s settings.xml deploy
  secrets:
    - NEXUS_USER
    - NEXUS_TOKEN
```

`settings.xml` lives in the repo and references env vars (`${env.NEXUS_USER}`)
that the platform's secret resolver injects. The
[`nexus`](/gocdnext/docs/reference/plugins/#nexus) plugin is an
alternative for pure-upload steps that don't run Maven.

## Common pitfalls

- **Daemon JVMs hold the cache lock**: avoid `-Dmaven.daemon.jvm`
  inside CI; the daemon survives the container exit and the next
  run errors with `Cannot lock .m2/repository`. Plain `mvn`
  invocations are fine.
- **`-T` flag with snapshots**: parallel builds + snapshot deps
  can race on `mvn install`. Stick to `-T 1` when the project's
  internals depend on each other's just-installed snapshots.
- **Repo size**: a `.m2/` for a typical Spring Boot app sits at
  500-800 MB. Set the gocdnext cache project quota generous in the
  Helm chart — see `caches.projectQuotaBytes` in
  [Helm install](/gocdnext/docs/install/helm/).
