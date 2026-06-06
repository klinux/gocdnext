---
title: Docker build & push
description: Build a multi-arch image, scan it, sign it, push to a registry — covering buildx, kaniko, cosign, trivy.
---

The container build chain has more moving parts than any other
recipe in the catalog. This walks the production-grade path:
multi-arch buildx via the agent's docker socket, scan with trivy,
sign with cosign, push to GHCR (or any OCI registry).

For rootless / Kubernetes-native builds without docker.sock, swap
buildx for kaniko at the bottom of the page.

## The full pipeline

```yaml title=".gocdnext/release.yaml"
name: release

when:
  event: [push, tag]
  branch: [main]

stages: [build, scan, sign, publish]

jobs:
  build:
    stage: build
    uses: gocdnext/buildx@v1
    docker: true                      # mount the host docker.sock
    secrets: [GHCR_USERNAME, GHCR_TOKEN]
    with:
      image: ghcr.io/klinux/myapp
      tags: |
        ${CI_COMMIT_SHORT_SHA}
        ${CI_BRANCH}
      context: .
      dockerfile: Dockerfile
      platforms: linux/amd64,linux/arm64
      cache-from: type=gha,scope=myapp
      cache-to: type=gha,scope=myapp,mode=max
      push: "false"                   # build now, push after scan+sign
      registry: ghcr.io
      username: ${{ GHCR_USERNAME }}
      password: ${{ GHCR_TOKEN }}

  trivy-scan:
    stage: scan
    uses: gocdnext/trivy@v1
    docker: true
    needs: [build]
    with:
      scan-type: image
      target: ghcr.io/klinux/myapp:${CI_COMMIT_SHORT_SHA}
      severity: HIGH,CRITICAL
      exit-code: "1"                  # fail the run on any HIGH/CRITICAL
      ignore-unfixed: "true"          # skip CVEs without a patch upstream

  cosign-sign:
    stage: sign
    uses: gocdnext/cosign@v1
    docker: true
    needs: [trivy-scan]
    secrets: [COSIGN_PRIVATE_KEY, COSIGN_PASSWORD]
    with:
      image: ghcr.io/klinux/myapp:${CI_COMMIT_SHORT_SHA}
      action: sign
      key: ${{ COSIGN_PRIVATE_KEY }}
      key-password: ${{ COSIGN_PASSWORD }}

  push:
    stage: publish
    uses: gocdnext/docker-push@v1
    docker: true
    needs: [cosign-sign]
    secrets: [GHCR_USERNAME, GHCR_TOKEN]
    with:
      source: ghcr.io/klinux/myapp:${CI_COMMIT_SHORT_SHA}
      target: ghcr.io/klinux/myapp
      tags: |
        ${CI_COMMIT_SHORT_SHA}
        ${CI_BRANCH}
        latest
      registry: ghcr.io
      username: ${{ GHCR_USERNAME }}
      password: ${{ GHCR_TOKEN }}
```

What's worth highlighting:

### `docker: true`

Mounts the host docker.sock + `host.docker.internal` alias inside
the job container. Required for buildx, trivy image scan, cosign
sign, and docker-push — all four shell out to the docker CLI.

### `push: "false"` until after scan + sign

Order matters: build → scan → sign → push. If trivy finds a
CRITICAL CVE, the run fails before the unsigned image ever
reaches the registry. Same for cosign — better to fail signing
than to push an unsigned tag.

The build stage stamps the image into the local daemon so the
subsequent jobs can reference it by tag.

### `cache-from` / `cache-to` with GHA

The `type=gha` GitHub Actions cache backend is supported by
buildx — same scope namespace, same hits. If you're not on
GitHub-hosted runners, swap to `type=registry,ref=...` or use the
plugin's shorthand `cache: ghcr.io/klinux/cache` (see [Container
layer cache](/gocdnext/docs/pipelines/recipes/layer-cache/) for
the full helper).

### Cosign keyless

Replace the secrets-based signing with keyless OIDC if your
gocdnext is on a public network and your registry supports
Sigstore Fulcio:

```yaml
cosign-sign:
  stage: sign
  uses: gocdnext/cosign@v1
  needs: [trivy-scan]
  with:
    image: ghcr.io/klinux/myapp:${CI_COMMIT_SHORT_SHA}
    action: sign
    cert-identity: "https://github.com/klinux/myapp/.github/workflows/release.yaml@refs/heads/main"
    cert-oidc-issuer: "https://token.actions.githubusercontent.com"
```

No secrets, no key rotation. The signature lands in the public
Rekor transparency log automatically.

## Variant — kaniko (no docker.sock)

For Kubernetes-native deployments where exposing docker.sock isn't
acceptable, [`gocdnext/kaniko`](/gocdnext/docs/reference/plugins/#kaniko)
builds inside an unprivileged container. Kaniko pushes to one
destination per job (single `image:`) — for multi-tag publishing
use the `docker-push` plugin after kaniko.

```yaml
build:
  stage: build
  uses: gocdnext/kaniko@v1
  secrets: [GHCR_USERNAME, GHCR_TOKEN]
  with:
    image: ghcr.io/klinux/myapp:${CI_COMMIT_SHORT_SHA}
    context: .
    dockerfile: Dockerfile
    cache: "true"
    registry: ghcr.io
    username: ${{ GHCR_USERNAME }}
    password: ${{ GHCR_TOKEN }}
```

Trade-off: kaniko is slower than buildx (no native multi-arch
acceleration, less aggressive caching), but it doesn't need
privileged daemon access. Pick based on your security posture.

## Variant — push only on tag

Per-job `when:` filtering isn't enforced today. The clean
separation is a **separate publish pipeline** triggered only on
tag — same image is built by `.gocdnext/release.yaml` on every
push, and `.gocdnext/publish.yaml` only fires when a tag arrives:

```yaml title=".gocdnext/publish.yaml"
name: publish
when:
  event: [tag]
stages: [push]
jobs:
  push:
    stage: push
    uses: gocdnext/docker-push@v1
    docker: true
    secrets: [GHCR_USERNAME, GHCR_TOKEN]
    with:
      source: ghcr.io/klinux/myapp:${CI_COMMIT_SHORT_SHA}
      target: ghcr.io/klinux/myapp
      tags: |
        ${CI_COMMIT_SHORT_SHA}
        latest
      registry: ghcr.io
      username: ${{ GHCR_USERNAME }}
      password: ${{ GHCR_TOKEN }}
```

## Common pitfalls

- **`docker: true` is privileged-ish**: the agent's docker.sock
  IS root in the host. Don't expose this to untrusted pipelines.
  Project secrets keep registry creds masked, but the build
  context itself is what runs.
- **Multi-arch on x86 agents**: buildx with QEMU emulation works
  but is slow on arm64 (~3-5× x86 build time). If you ship
  multi-arch frequently, dedicate an arm64 agent in the runner
  pool — agent `tags:` can route arm64 builds to it.
- **Trivy image scans need a pulled image**: `target:` must
  reference an image already in the daemon (the previous
  `build` job's output). For pre-build scans of FROM
  references, use `scan-type: fs` against the Dockerfile dir
  instead.
- **Cosign + signed manifests + push order**: cosign signs the
  manifest IN the registry. If you sign before push, the
  signature has no manifest to attach to. The recipe above
  signs against the local daemon (`docker: true` lets cosign
  see it) — most current cosign versions accept that.
