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

Three plugins handle this end-to-end:
[`gocdnext/tag`](/gocdnext/docs/reference/plugins/#tag),
[`gocdnext/release-notes`](/gocdnext/docs/reference/plugins/#release-notes),
[`gocdnext/github-release`](/gocdnext/docs/reference/plugins/#github-release).

## The pipeline

```yaml title=".gocdnext/release.yaml"
name: release

when:
  event: [tag]                    # only fires on a vX.Y.Z tag push
  tag_name: '^v\d+\.\d+\.\d+$'    # strict: ignores -rc, -beta tags

stages: [build, package, publish]

jobs:
  build-linux-amd64:
    stage: build
    uses: gocdnext/go@v1
    with:
      command: build -ldflags "-X main.Version=${TAG_NAME}" -o dist/myapp-linux-amd64 ./cmd/myapp
    variables:
      GOOS: linux
      GOARCH: amd64
    artifacts:
      paths: [dist/myapp-linux-amd64]

  build-linux-arm64:
    stage: build
    uses: gocdnext/go@v1
    with:
      command: build -ldflags "-X main.Version=${TAG_NAME}" -o dist/myapp-linux-arm64 ./cmd/myapp
    variables:
      GOOS: linux
      GOARCH: arm64
    artifacts:
      paths: [dist/myapp-linux-arm64]

  build-darwin-arm64:
    stage: build
    uses: gocdnext/go@v1
    with:
      command: build -ldflags "-X main.Version=${TAG_NAME}" -o dist/myapp-darwin-arm64 ./cmd/myapp
    variables:
      GOOS: darwin
      GOARCH: arm64
    artifacts:
      paths: [dist/myapp-darwin-arm64]

  checksums:
    stage: package
    uses: gocdnext/go@v1
    needs: [build-linux-amd64, build-linux-arm64, build-darwin-arm64]
    needs_artifacts:
      - from_job: build-linux-amd64
        paths: [dist/]
      - from_job: build-linux-arm64
        paths: [dist/]
      - from_job: build-darwin-arm64
        paths: [dist/]
    image: alpine:3.20
    script:
      - cd dist && sha256sum myapp-* > SHA256SUMS
    artifacts:
      paths: [dist/SHA256SUMS]

  notes:
    stage: package
    uses: gocdnext/release-notes@v1
    with:
      # Range from the previous tag to the current one. The plugin
      # walks `git log` and groups by Conventional Commit type:
      #   feat:    → "Features"
      #   fix:     → "Bug fixes"
      #   chore:   → "Chores"
      #   docs:    → "Documentation"
      from_ref: previous_tag         # auto-resolved from the git tag list
      to_ref: ${TAG_NAME}
      output: dist/notes.md

  publish:
    stage: publish
    uses: gocdnext/github-release@v1
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
    with:
      tag: ${TAG_NAME}
      title: "myapp ${TAG_NAME}"
      notes_file: dist/notes.md
      attach: |
        dist/myapp-linux-amd64
        dist/myapp-linux-arm64
        dist/myapp-darwin-arm64
        dist/SHA256SUMS
    secrets:
      - GH_RELEASE_TOKEN          # PAT with contents:write
```

What's worth highlighting:

### Strict `tag_name:` regex

`^v\d+\.\d+\.\d+$` matches `v0.2.0`, `v1.0.0`. It rejects
`v0.2.0-rc1`, `v0.2.0-beta`, etc. — those would fire the same
release flow against a pre-release tag, which is rarely what you
want. Pre-release flows usually need a different pipeline (skip
the published step, or publish as a draft).

For pre-release support, change the regex to
`^v\d+\.\d+\.\d+(-rc\d+)?$` and gate downstream:

```yaml
publish:
  when:
    tag_name: '^v\d+\.\d+\.\d+$'      # strict gate at the publish step
```

### Three parallel build jobs

The build jobs are independent — they fan out across whatever
agents are free. On a 3-agent pool, all three platforms compile
in parallel; on a 1-agent pool they serialise. The pipeline shape
doesn't change.

For more platforms (Windows, freebsd, more arches), add more jobs.
A matrix expansion is more compact:

```yaml
build:
  stage: build
  uses: gocdnext/go@v1
  matrix:
    target:
      - { goos: linux, goarch: amd64 }
      - { goos: linux, goarch: arm64 }
      - { goos: darwin, goarch: arm64 }
      - { goos: darwin, goarch: amd64 }
      - { goos: windows, goarch: amd64 }
  variables:
    GOOS: ${{ matrix.target.goos }}
    GOARCH: ${{ matrix.target.goarch }}
  with:
    command: build -ldflags "-X main.Version=${TAG_NAME}" -o dist/myapp-${{ matrix.target.goos }}-${{ matrix.target.goarch }} ./cmd/myapp
  artifacts:
    paths: [dist/]
```

### Release notes from Conventional Commits

The `release-notes` plugin groups commits by the type prefix:

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

## Bug fixes
- correct off-by-one in bar

## Documentation
- (api) document new endpoint
```

The chore is filtered out by default. Custom mapping is configurable
via `.release-notes.yaml` at the repo root.

### `from_ref: previous_tag`

The plugin auto-resolves to whatever the previous semver-shaped
tag was. For the very first release (`v0.1.0` with no prior tag),
it falls back to the initial commit — the notes capture the
project's full history.

## Variations

### Draft release for review

Push the release as a draft, let a human review the notes, click
publish manually:

```yaml
publish:
  uses: gocdnext/github-release@v1
  with:
    ...
    draft: true
```

The release exists on GitHub but isn't visible to consumers until
manually published. Useful for orgs with mandatory release sign-off.

### Automated changelog commit

After the release lands, you might want to commit the generated
notes back to `CHANGELOG.md`:

```yaml
update-changelog:
  stage: publish
  needs: [publish]
  image: alpine:3.20
  script:
    - apk add git
    - git config user.email "ci@example.com"
    - git config user.name "CI"
    - cat dist/notes.md > CHANGELOG.md.new
    - cat CHANGELOG.md >> CHANGELOG.md.new
    - mv CHANGELOG.md.new CHANGELOG.md
    - git checkout main
    - git add CHANGELOG.md
    - git commit -m "chore: changelog for ${TAG_NAME}"
    - git push
  secrets:
    - GIT_PUSH_TOKEN
```

This needs the same PAT as the release publish — careful, it
opens an automation surface. Many teams skip this step and just
keep the changelog in the GitHub Release notes.

### Sign release artefacts

```yaml
sign:
  stage: package
  needs: [build-linux-amd64, build-linux-arm64, build-darwin-arm64]
  uses: gocdnext/cosign@v1
  with:
    command: sign-blob --yes --output-signature dist/myapp.sig dist/myapp-*
  variables:
    COSIGN_EXPERIMENTAL: "1"
```

Then attach `dist/myapp.sig` to the release. Consumers verify
with `cosign verify-blob`.

## Common pitfalls

- **`-ldflags` quoting**: the `-X main.Version=${TAG_NAME}` flag
  is a single shell arg. The plugin's `command:` is word-split,
  so the entire flag must NOT contain spaces between `=` and the
  value. If you need spaces (`"version: 1.0"`), quote inline:
  `-ldflags '-X "main.Version=v 1.0"'`.
- **Cross-compilation needs CGO=0**: `CGO_ENABLED=1` (the default
  in some images) requires C toolchain for the target — fails
  cross-compile. Set `CGO_ENABLED=0` in `variables:` unless you
  actually need cgo.
- **Tag must be pushed AFTER the commit**: gocdnext fires on tag
  push; if you tag before pushing the commit, the run lands on a
  SHA the public GitHub doesn't know about and the release notes
  step misbehaves. Push commit first, tag second.
- **Reusing release tags**: don't. The `github-release` plugin
  refuses to overwrite an existing release by default. Force-push
  via `replace: true` is supported but discouraged — published
  releases should be immutable.
