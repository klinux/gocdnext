---
title: Helm chart release
description: Lint, package, push a Helm chart to an OCI registry on tag pushes.
---

This is the recipe gocdnext itself uses for `charts/gocdnext`.
Lint on every push, package + publish only when a tag fires.

Per-job `when:` filtering isn't enforced today. The clean
separation is **two pipeline files** — `.gocdnext/chart-lint.yaml`
runs every push to validate the chart; `.gocdnext/chart-release.yaml`
fires only on tag pushes and does the publish.

## Layout assumed

```
repo/
├── charts/
│   └── myapp/
│       ├── Chart.yaml
│       ├── values.yaml
│       └── templates/...
└── .gocdnext/
    ├── chart-lint.yaml
    └── chart-release.yaml
```

## Lint pipeline (every push)

```yaml title=".gocdnext/chart-lint.yaml"
name: chart-lint

when:
  event: [push, pull_request]

stages: [lint]

jobs:
  lint:
    stage: lint
    uses: ghcr.io/klinux/gocdnext-plugin-helm@v1
    with:
      command: lint charts/myapp

  template:
    stage: lint
    uses: ghcr.io/klinux/gocdnext-plugin-helm@v1
    with:
      command: template myapp charts/myapp --debug
```

`helm lint` catches schema errors; `helm template` actually
renders and catches YAML / template-logic errors lint misses
(unbalanced `if`, undefined `.Values.foo`, etc.). Running both
catches more before the chart ever leaves the build host.

## Release pipeline (tag only)

```yaml title=".gocdnext/chart-release.yaml"
name: chart-release

when:
  event: [tag]                    # tag pushes only

stages: [package, publish]

jobs:
  package:
    stage: package
    uses: ghcr.io/klinux/gocdnext-plugin-helm@v1
    with:
      # Stamp version + appVersion from the tag (CI_BRANCH carries
      # the tag ref on a tag push). The plugin's `command:` is
      # word-split — keep flag pairs on one line.
      command: package charts/myapp --version ${CI_BRANCH} --app-version ${CI_BRANCH} --destination dist/
    artifacts:
      paths: ["dist/*.tgz"]

  publish-oci:
    stage: publish
    uses: ghcr.io/klinux/gocdnext-plugin-helm-push@v1
    needs: [package]
    needs_artifacts:
      - from_job: package
        paths: ["dist/"]
    secrets: [GHCR_USERNAME, GHCR_TOKEN]
    with:
      chart-dir: charts/myapp
      version: ${CI_BRANCH}
      app-version: ${CI_BRANCH}
      backend: oci
      oci-repo: oci://ghcr.io/klinux/charts
      username: ${{ GHCR_USERNAME }}
      password: ${{ GHCR_TOKEN }}
```

What's worth highlighting:

### `helm-push` backends

The plugin supports `oci` (default — GHCR, Docker Hub, ECR,
GAR), `chartmuseum`, and `nexus`. The choice is one `backend:`
input — same `helm package` step regardless, the publish flow
swaps transport.

GHCR: `backend: oci`, `oci-repo: oci://ghcr.io/<owner>/charts`.
ChartMuseum: `backend: chartmuseum`, `repo-url:
https://charts.internal/`. Nexus: `backend: nexus`, `repo-url:
https://nexus.corp/repository/helm/`.

The `gh-pages` HTTP-repo pattern (chart-releaser-style index
merge) isn't supported by this plugin — use GitHub Actions for
that publish path or migrate consumers to OCI.

### Version from the tag

The plugin doesn't strip a `v` prefix from `CI_BRANCH` on tag
pushes — so a tag `v0.6.4` becomes the chart version `v0.6.4`.
Most consumers tolerate that; if you require strict semver
(`0.6.4`), strip the prefix yourself in a pre-step that rewrites
`Chart.yaml` before the package job.

### Tag fires both pipelines

A `vX.Y.Z` push fires `chart-lint.yaml` (lint + template) AND
`chart-release.yaml` (package + publish). The lint pipeline
catches a broken render before the release pipeline pushes a
broken artefact. If the lint pipeline fails, the release
pipeline's package job still runs (they're independent
pipelines) — guard the publish step on lint success via an
`upstream:` material:

```yaml
materials:
  - upstream:
      pipeline: chart-lint
      stage: lint
      status: success
```

## Variations

### Sign the chart with cosign

OCI charts can be signed the same way as OCI images. Consumers
verify with `cosign verify`.

```yaml
sign-chart:
  stage: publish
  uses: ghcr.io/klinux/gocdnext-plugin-cosign@v1
  needs: [publish-oci]
  secrets: [COSIGN_PRIVATE_KEY, COSIGN_PASSWORD, GHCR_USERNAME, GHCR_TOKEN]
  with:
    # `key-content:` accepts the inline PEM from secrets; the
    # plugin writes it to a 0600 tempfile internally and a
    # `trap` wipes it on exit. The `key:` input is a FILE PATH
    # and the plugin refuses PEM-like values via a guard. No
    # `docker: true` — cosign signs via registry API.
    image: ghcr.io/klinux/charts/myapp:${CI_BRANCH}
    action: sign
    key-content: ${{ COSIGN_PRIVATE_KEY }}
    key-password: ${{ COSIGN_PASSWORD }}
    registry: ghcr.io
    username: ${{ GHCR_USERNAME }}
    password: ${{ GHCR_TOKEN }}
```

### Auto-bump appVersion from a sibling pipeline

If your container image is built by a different pipeline (and
the chart's `appVersion` should match the pushed image tag),
gate the chart release on the image's release as an `upstream`
material:

```yaml
materials:
  - upstream:
      pipeline: release
      stage: publish
      status: success
```

The chart pipeline now waits for the image release to land.
Useful when the image and chart live in the same repo but ship
in separate cycles.

## Common pitfalls

- **`helm push` to OCI requires login**: `GHCR_TOKEN` should be a
  PAT with `write:packages`. The plugin runs `helm registry
  login` with the secret before the push.
- **`Chart.yaml` stays at the in-repo version**: the package step
  stamps the artefact's version from the tag via
  `--version`/`--app-version`, but `Chart.yaml` in main stays at
  whatever was last committed. Convention is to bump `Chart.yaml`
  to the next planned release in a chore commit before tagging.
- **`v`-prefix in OCI tags**: GHCR accepts `v0.6.4` as a chart
  version tag, but some clients pin via plain SemVer
  (`helm install --version 0.6.4`). Choose one convention for
  the org and stick to it.
- **gh-pages publish migration**: the chart-releaser-style HTTP
  publish isn't supported by `gocdnext/helm-push`. Operators
  migrating off that flow either move consumers to OCI or keep
  publishing via GitHub Actions until OCI is universal.
