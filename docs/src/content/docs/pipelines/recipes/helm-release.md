---
title: Helm chart release
description: Lint, package, push a Helm chart to OCI + the gh-pages-style HTTP repo, with version stamping from a release tag.
---

This is the recipe gocdnext itself uses for `charts/gocdnext` —
[`.github/workflows/release.yml`](https://github.com/klinux/gocdnext/blob/main/.github/workflows/release.yml).
Lint on every push, package + publish only when a `v0.X.Y` tag
fires.

## Layout assumed

```
repo/
├── charts/
│   └── myapp/
│       ├── Chart.yaml
│       ├── values.yaml
│       └── templates/...
└── .gocdnext/
    └── chart.yaml
```

## The pipeline

```yaml title=".gocdnext/chart.yaml"
name: chart

when:
  event: [push, pull_request, tag]

stages: [lint, package, publish]

jobs:
  lint:
    stage: lint
    uses: gocdnext/helm@v1
    with:
      command: lint charts/myapp

  template:
    stage: lint
    uses: gocdnext/helm@v1
    with:
      command: template myapp charts/myapp --debug

  package:
    stage: package
    uses: gocdnext/helm@v1
    needs: [lint, template]
    with:
      # On a tag push, stamp version + appVersion from the tag.
      # On a branch push, leave Chart.yaml's value alone (linted
      # only, not published).
      command: |
        package charts/myapp
        ${TAG_NAME:+--version "${TAG_NAME#v}"}
        ${TAG_NAME:+--app-version "${TAG_NAME#v}"}
        --destination dist/
    artifacts:
      paths: ["dist/*.tgz"]

  publish-oci:
    stage: publish
    uses: gocdnext/helm-push@v1
    needs: [package]
    needs_artifacts:
      - from_job: package
        paths: ["dist/"]
    when:
      event: [tag]
    with:
      package_glob: "dist/*.tgz"
      registry: oci://ghcr.io/${OWNER}/charts
    secrets:
      - GHCR_TOKEN
    variables:
      OWNER: klinux

  publish-http:
    stage: publish
    uses: gocdnext/helm-push@v1
    needs: [package]
    needs_artifacts:
      - from_job: package
        paths: ["dist/"]
    when:
      event: [tag]
    with:
      package_glob: "dist/*.tgz"
      mode: gh-pages
      pages_url: https://${OWNER}.github.io/${REPO}
    secrets:
      - GH_PAGES_TOKEN          # PAT with contents:write on the same repo
    variables:
      OWNER: klinux
      REPO: myapp
```

What's worth highlighting:

### `lint` + `template` are parallel

`helm lint` catches schema errors; `helm template` actually renders
the chart and catches YAML/template logic errors that lint misses
(unbalanced `if`, undefined `.Values.foo`, etc.). Running both
catches more before the chart ever leaves the build host.

### Tag-only publish

`when.event: [tag]` on the publish jobs means branches build +
package the chart (so PR pipelines verify the package step works)
but don't ship a tarball anywhere. Tags trigger the actual push.

### Two destinations

OCI (`oci://ghcr.io/klinux/charts`) and HTTP (`gh-pages`-served
`https://klinux.github.io/myapp/`) are both supported because
operators have preferences. OCI is the modern path (single
destination, `helm install oci://...` works directly); the HTTP
repo is what `helm repo add` consumers expect. Publishing to both
keeps everyone happy with one pipeline.

### `${TAG_NAME#v}` strips the v prefix

Tag is `v0.2.0`; chart version expects `0.2.0`. Bash parameter
expansion (`#v`) drops the leading `v`. The `${TAG_NAME:+...}`
guard skips the flag entirely on non-tag pushes — `helm package`
without `--version` uses what's in `Chart.yaml`.

## Variations

### With chart-releaser pattern (gh-pages merge)

The vanilla helm-push wipes the gh-pages branch's index. To
preserve all prior versions, use the chart-releaser-style merge:

```yaml
publish-http:
  uses: gocdnext/helm-push@v1
  with:
    package_glob: "dist/*.tgz"
    mode: gh-pages
    pages_url: https://klinux.github.io/myapp
    merge_existing_index: true   # default in v1; explicit here for clarity
```

`merge_existing_index: true` fetches the current `index.yaml`
from gh-pages, appends your new tarball, regenerates. Prior
releases keep working.

### Sign the chart with cosign

```yaml
sign-chart:
  stage: publish
  uses: gocdnext/cosign@v1
  needs: [publish-oci]
  when:
    event: [tag]
  with:
    command: sign --yes ghcr.io/klinux/charts/myapp:${TAG_NAME#v}
  variables:
    COSIGN_EXPERIMENTAL: "1"
```

OCI charts can be signed the same way as OCI images. Consumers
verify with `cosign verify`.

### Auto-bump appVersion from a sibling pipeline

If your image is built by a different pipeline (`release` above)
and the chart's `appVersion` should match the pushed image tag,
gate the chart job on the image's release as an `upstream`
material:

```yaml
materials:
  - upstream:
      pipeline: release
      stage: publish
      status: success
```

The chart pipeline now waits for the image release to land. Useful
when the image and chart live in the same repo but ship in
separate cycles.

## Common pitfalls

- **`helm push` to OCI requires login**: GHCR_TOKEN should be a
  PAT with `write:packages`. The plugin's entrypoint runs `helm
  registry login` with the secret before the push.
- **gh-pages merge race**: the merge step does a
  `git fetch && git rebase` against gh-pages. If two chart
  releases race, the second is rejected. Add a `concurrency`
  group at the workflow level to serialise — gocdnext's parser
  doesn't support workflow-level concurrency yet, so for now
  serialize with materials or a lock job.
- **Chart.yaml stays at the in-repo version**: the package step
  stamps the artefact's version from the tag, but `Chart.yaml`
  in main stays at whatever was last committed. Convention is to
  bump `Chart.yaml` to the next planned release in a chore
  commit before tagging — the tag stamp re-stamps it on the
  artefact regardless.
