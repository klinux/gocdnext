---
title: Release flow (tag → release notes → GitHub Release)
description: Cut a versioned release — derive notes from commits, create the GitHub Release, attach binaries.
---

A clean release flow does three things on a `vX.Y.Z` tag push:

1. Derive release notes from commits since the last tag
   (Conventional Commits format works best).
2. Create a GitHub Release with those notes attached.
3. Upload the release artefacts (binaries, checksums) to the
   Release.

Two plugins handle this end-to-end:
[`gocdnext/release-notes`](/gocdnext/docs/reference/plugins/#release-notes),
[`gocdnext/github-release`](/gocdnext/docs/reference/plugins/#github-release).

The pipeline-level `when:` filter accepts `event:` and `branch:`
only — there's no `tag_name:` regex today. Gate on `event: [tag]`
and trust the build only fires on tag pushes; finer regex
filtering would need to happen inside a build job (rare).

## The pipeline

```yaml title=".gocdnext/release.yaml"
name: release

when:
  event: [tag]                    # only fires on tag pushes

stages: [build, package, publish]

jobs:
  build-linux-amd64:
    stage: build
    uses: ghcr.io/klinux/gocdnext-plugin-go@v1
    variables:
      GOOS: linux
      GOARCH: amd64
    with:
      command: build -ldflags "-X main.Version=${CI_BRANCH}" -o dist/myapp-linux-amd64 ./cmd/myapp
    artifacts:
      paths: [dist/myapp-linux-amd64]

  build-linux-arm64:
    stage: build
    uses: ghcr.io/klinux/gocdnext-plugin-go@v1
    variables:
      GOOS: linux
      GOARCH: arm64
    with:
      command: build -ldflags "-X main.Version=${CI_BRANCH}" -o dist/myapp-linux-arm64 ./cmd/myapp
    artifacts:
      paths: [dist/myapp-linux-arm64]

  build-darwin-arm64:
    stage: build
    uses: ghcr.io/klinux/gocdnext-plugin-go@v1
    variables:
      GOOS: darwin
      GOARCH: arm64
    with:
      command: build -ldflags "-X main.Version=${CI_BRANCH}" -o dist/myapp-darwin-arm64 ./cmd/myapp
    artifacts:
      paths: [dist/myapp-darwin-arm64]

  checksums:
    stage: package
    image: alpine:3.20
    needs: [build-linux-amd64, build-linux-arm64, build-darwin-arm64]
    needs_artifacts:
      - from_job: build-linux-amd64
        paths: [dist/]
      - from_job: build-linux-arm64
        paths: [dist/]
      - from_job: build-darwin-arm64
        paths: [dist/]
    script:
      - cd dist && sha256sum myapp-* > SHA256SUMS
    artifacts:
      paths: [dist/SHA256SUMS]

  notes:
    stage: package
    uses: ghcr.io/klinux/gocdnext-plugin-release-notes@v1
    with:
      # Default `from:` walks back to the nearest prior tag via
      # `git describe --tags --abbrev=0`; first-release repos fall
      # back to the root commit so the very first release still
      # produces notes. `to:` defaults to HEAD.
      output: dist/notes.md
      format: conventional
      heading: "## ${CI_BRANCH}"
    artifacts:
      paths: [dist/notes.md]

  publish:
    stage: publish
    uses: ghcr.io/klinux/gocdnext-plugin-github-release@v1
    needs: [checksums, notes]
    needs_artifacts:
      - from_job: build-linux-amd64
        paths: [dist/]
      - from_job: build-linux-arm64
        paths: [dist/]
      - from_job: build-darwin-arm64
        paths: [dist/]
      - from_job: checksums
        paths: [dist/]
      - from_job: notes
        paths: [dist/]
    secrets: [GH_RELEASE_TOKEN]      # PAT with contents:write
    with:
      tag: ${CI_BRANCH}              # CI_BRANCH carries the ref the tag push hit
      title: "myapp ${CI_BRANCH}"
      token: ${{ GH_RELEASE_TOKEN }}
      assets: |
        dist/myapp-linux-amd64
        dist/myapp-linux-arm64
        dist/myapp-darwin-arm64
        dist/SHA256SUMS
      # github-release `notes:` is inline body text. Read the
      # release-notes file into it by composing — see the variation
      # below for the alpine cat-into-env pattern.
      generate-notes: "true"
```

What's worth highlighting:

### Three parallel build jobs

The build jobs are independent — they fan out across whatever
agents are free. On a 3-agent pool, all three platforms compile
in parallel; on a 1-agent pool they serialise. The pipeline shape
doesn't change.

For more platforms (Windows, freebsd, more arches), add more jobs
or use `parallel.matrix:` (the list-of-objects shape):

```yaml
build:
  stage: build
  uses: ghcr.io/klinux/gocdnext-plugin-go@v1
  parallel:
    matrix:
      - GOOS: [linux, darwin]
        GOARCH: [amd64, arm64]
      - GOOS: [windows]
        GOARCH: [amd64]
  variables:
    GOOS: ${{ GOOS }}
    GOARCH: ${{ GOARCH }}
  with:
    command: build -ldflags "-X main.Version=${CI_BRANCH}" -o dist/myapp-${{ GOOS }}-${{ GOARCH }} ./cmd/myapp
  artifacts:
    paths: [dist/]
```

`parallel.matrix:` is a list of objects; each object maps a name
to a list of values. The cartesian product across both keys in
the first entry gives 4 cells, plus the second entry's 1 cell = 5
build jobs total.

### Release notes from Conventional Commits

`format: conventional` groups commits by the type prefix:

```
feat: add support for foo
fix: correct off-by-one in bar
chore: bump deps
docs(api): document new endpoint
```

Becomes:

```markdown
## Features
- add support for foo

## Bug Fixes
- correct off-by-one in bar

## Chores
- bump deps

## Other
- docs(api) document new endpoint
```

Unclassifiable commits land under "Other" so nothing gets
dropped.

### `from:` auto-resolution

The plugin's default `from:` is "the nearest prior tag" via `git
describe --tags --abbrev=0`. On a never-tagged repo it falls
back to the root commit so the very first release still produces
notes.

## Variations

### Feed release notes into the GitHub Release body

`github-release.notes:` is inline body text. To pull the
release-notes file in, render it through a shell job:

```yaml
publish:
  stage: publish
  image: alpine:3.20
  needs: [checksums, notes]
  needs_artifacts:
    - from_job: build-linux-amd64
      paths: [dist/]
    - from_job: build-linux-arm64
      paths: [dist/]
    - from_job: build-darwin-arm64
      paths: [dist/]
    - from_job: checksums
      paths: [dist/]
    - from_job: notes
      paths: [dist/]
  secrets: [GH_RELEASE_TOKEN]
  script:
    - apk add --no-cache curl
    - |
      BODY=$(cat dist/notes.md)
      curl -fSL -X POST -H "Authorization: token ${GH_RELEASE_TOKEN}" \
        -H "Accept: application/vnd.github+json" \
        https://api.github.com/repos/klinux/myapp/releases \
        -d @- <<EOF
      {"tag_name":"${CI_BRANCH}","name":"myapp ${CI_BRANCH}","body":$(jq -Rs . <<< "$BODY")}
      EOF
```

This bypasses the `github-release` plugin's lack of a `notes-file:`
input. Use it when you want the conventional-commits body
verbatim in the Release page.

### Draft release for review

Push the release as a draft, let a human review the notes, click
publish manually:

```yaml
publish:
  uses: ghcr.io/klinux/gocdnext-plugin-github-release@v1
  secrets: [GH_RELEASE_TOKEN]
  with:
    tag: ${CI_BRANCH}
    token: ${{ GH_RELEASE_TOKEN }}
    draft: "true"
```

The release exists on GitHub but isn't visible to consumers until
manually published. Useful for orgs with mandatory release sign-off.

### Sign release artefacts (keyless)

```yaml
sign:
  stage: package
  needs: [build-linux-amd64, build-linux-arm64, build-darwin-arm64]
  uses: ghcr.io/klinux/gocdnext-plugin-cosign@v1
  needs_artifacts:
    - from_job: build-linux-amd64
      paths: [dist/]
    - from_job: build-linux-arm64
      paths: [dist/]
    - from_job: build-darwin-arm64
      paths: [dist/]
  with:
    # cosign's blob-signing path lives in `action: sign-blob` —
    # confirm the version of the plugin shipping that action
    # before relying on it. Image signing uses `action: sign` +
    # `image:`.
    image: dist/myapp-linux-amd64
    action: sign
    cert-identity: "https://github.com/klinux/myapp/.github/workflows/release.yaml@refs/heads/main"
    cert-oidc-issuer: "https://token.actions.githubusercontent.com"
```

Then attach the resulting `.sig` to the release. Consumers verify
with `cosign verify-blob`.

## Common pitfalls

- **`-ldflags` quoting**: the `-X main.Version=${CI_BRANCH}` flag
  is a single shell arg. The plugin's `command:` is word-split,
  so the entire flag must NOT contain spaces between `=` and the
  value. If you need spaces (`"version: 1.0"`), quote inline:
  `-ldflags '-X "main.Version=v 1.0"'`.
- **Cross-compilation needs `CGO_ENABLED=0`**: `CGO_ENABLED=1`
  (the default in some images) requires a C toolchain for the
  target — fails cross-compile. Set `CGO_ENABLED=0` in
  `variables:` unless you actually need cgo.
- **Tag must be pushed AFTER the commit**: gocdnext fires on tag
  push; if you tag before pushing the commit, the run lands on a
  SHA the public GitHub doesn't know about and the release notes
  step misbehaves. Push commit first, tag second.
- **Reusing release tags**: don't. The `github-release` plugin
  refuses to overwrite an existing release by default. Published
  releases should be immutable; bump the patch instead.
