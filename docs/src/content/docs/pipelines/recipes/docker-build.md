---
title: Docker build & push
description: Build a multi-arch image, publish it, scan with trivy, sign with cosign — covering buildx, kaniko, cosign, trivy.
---

The container build chain has more moving parts than any other
recipe in the catalog. This walks the production-grade path:
multi-arch buildx via the agent's docker socket publishing to the
registry, scan the published image with trivy, sign with cosign
via the registry API. The order is **build (push:true) → scan →
sign** — see [Scan-after-publish (and why)](#scan-after-publish-and-why)
for the trade-off discussion + alternatives.

For rootless / Kubernetes-native builds without docker.sock, swap
buildx for kaniko at the bottom of the page.

## The full pipeline

```yaml title=".gocdnext/release.yaml"
name: release

when:
  event: [push, tag]
  branch: [main]

stages: [build, scan, sign]

jobs:
  build:
    stage: build
    uses: ghcr.io/klinux/gocdnext-plugin-buildx@v1
    docker: true                      # mount the host docker.sock
    secrets: [GHCR_USERNAME, GHCR_TOKEN]
    with:
      image: ghcr.io/klinux/myapp
      tags: |
        ${CI_COMMIT_SHORT_SHA}
        ${CI_BRANCH}
        latest
      context: .
      dockerfile: Dockerfile
      platforms: linux/amd64,linux/arm64
      cache-from: type=gha,scope=myapp
      cache-to: type=gha,scope=myapp,mode=max
      push: "true"                    # scan-after-publish (see below)
      registry: ghcr.io
      username: ${{ GHCR_USERNAME }}
      password: ${{ GHCR_TOKEN }}

  trivy-scan:
    stage: scan
    uses: ghcr.io/klinux/gocdnext-plugin-trivy@v1
    needs: [build]
    # No `docker: true` — trivy pulls the image via the registry
    # API (it ships its own OCI client), so it doesn't need the
    # host docker socket. Keeping the socket off this job is
    # important because it carries the GHCR token: blast radius
    # of a token-bearing job + privileged docker socket is much
    # worse than either alone.
    #
    # Registry creds are required: the build job's `docker login`
    # doesn't survive across jobs (each job runs in a fresh
    # container), so trivy has to authenticate against the
    # registry on its own to pull the published image. The plugin
    # promotes `username:`/`password:` to TRIVY_USERNAME /
    # TRIVY_PASSWORD env which trivy reads natively.
    secrets: [GHCR_USERNAME, GHCR_TOKEN]
    with:
      # Scan the PUBLISHED image (registry-side). Multi-arch
      # manifest lists can't live in a local daemon, so the
      # alternative — "build to local with push:false, scan
      # there" — only works for single-arch. The trade-off of
      # scan-after-publish is documented below.
      scan-type: image
      target: ghcr.io/klinux/myapp:${CI_COMMIT_SHORT_SHA}
      severity: HIGH,CRITICAL
      exit-code: "1"                  # fail the run on any HIGH/CRITICAL
      ignore-unfixed: "true"          # skip CVEs without a patch upstream
      username: ${{ GHCR_USERNAME }}
      password: ${{ GHCR_TOKEN }}

  cosign-sign:
    stage: sign
    uses: ghcr.io/klinux/gocdnext-plugin-cosign@v1
    needs: [trivy-scan]
    secrets: [COSIGN_PRIVATE_KEY, COSIGN_PASSWORD, GHCR_USERNAME, GHCR_TOKEN]
    with:
      # `key-content:` accepts the inline PEM from secrets; the
      # plugin writes it to a 0600 tempfile internally and a
      # `trap` wipes it on exit. Don't use `key:` for secret
      # content — that input is a FILE PATH and the plugin will
      # refuse PEM-like values. No `docker: true` — cosign signs
      # via registry API; the manifest already lives there
      # because the build job pushed it.
      image: ghcr.io/klinux/myapp:${CI_COMMIT_SHORT_SHA}
      action: sign
      key-content: ${{ COSIGN_PRIVATE_KEY }}
      key-password: ${{ COSIGN_PASSWORD }}
      registry: ghcr.io
      username: ${{ GHCR_USERNAME }}
      password: ${{ GHCR_TOKEN }}
```

What's worth highlighting:

### `docker: true` — only where it's actually needed

Mounts the host docker.sock + `host.docker.internal` alias inside
the job container. Only the `build` job sets this — buildx
genuinely needs the daemon to assemble the multi-arch manifest.

`trivy-scan` and `cosign-sign` BOTH talk to the registry API
directly (trivy ships its own OCI client; cosign always operated
that way). Keeping the docker socket off those jobs matters
because they carry secret material — the registry token in
trivy's case, the cosign signing key in cosign's case. Blast
radius of a token-bearing job + privileged docker socket is much
worse than either alone.

### Scan-after-publish (and why)

The multi-arch build pushes directly with `push: "true"` because
buildx can't load a multi-platform manifest list into a local
daemon — it has to live in a registry. So the natural "build
locally → scan → sign → push" flow that works for single-arch is
unavailable for multi-arch. The recipe accepts the trade-off:

- The image is published BEFORE trivy verifies it has no
  HIGH/CRITICAL CVEs.
- If trivy fails, the run fails — and an operator runbook step
  needs to `oras delete` (or the registry-equivalent) of the now-
  published-but-failed tag. Acceptable in practice because scan
  failures should be rare on a properly maintained image; the
  window of exposure (build → trivy job, ~30s) is short.

If you need scan-before-publish AND multi-arch, the
[trunk-based-release concept doc](/gocdnext/docs/concepts/trunk-based-release/)
discusses the registry-side image-copy alternatives
(`crane copy` / `skopeo copy` / `buildx imagetools create`)
that the recipe here doesn't ship today.

The build stage publishes the multi-arch manifest list to the
registry so the subsequent jobs can reference it by tag against
the same registry — no shared local daemon between jobs needed,
which matches the workspace-isolation model of the Kubernetes
runner.

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
Sigstore Fulcio. Registry creds are still required: keyless
removes the SIGNING key, not the upload-the-signature step —
the signature manifest still has to be PUT into the registry,
which on a private registry needs auth.

```yaml
cosign-sign:
  stage: sign
  uses: ghcr.io/klinux/gocdnext-plugin-cosign@v1
  needs: [trivy-scan]
  secrets: [GHCR_USERNAME, GHCR_TOKEN]
  with:
    image: ghcr.io/klinux/myapp:${CI_COMMIT_SHORT_SHA}
    action: sign
    cert-identity: "https://github.com/klinux/myapp/.github/workflows/release.yaml@refs/heads/main"
    cert-oidc-issuer: "https://token.actions.githubusercontent.com"
    registry: ghcr.io
    username: ${{ GHCR_USERNAME }}
    password: ${{ GHCR_TOKEN }}
```

No private key to rotate. The signature lands in the public
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
  uses: ghcr.io/klinux/gocdnext-plugin-kaniko@v1
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

## On scan-before-publish

A previous variant of this recipe tried to keep the image off
the registry until trivy + cosign passed, using `push: "false"`
+ `docker: true` so the inspections could run against the local
daemon. That doesn't compose with the gocdnext/cosign plugin:
the plugin signs registry refs (it constructs the signature
manifest via the registry API), so a not-yet-published image
fails at the sign step regardless of `docker: true`.

If you genuinely need "no unscanned artefact ever reaches the
registry", the realistic options are:

- Build to a **staging registry** the team doesn't consume from,
  scan + sign + promote-to-prod registry with
  `crane copy --all-tags` (or `skopeo copy` / `buildx
  imagetools create`). This preserves multi-arch manifest lists
  during promotion — `gocdnext/docker-push` does NOT, see the
  [trunk-based-release concept doc](/gocdnext/docs/concepts/trunk-based-release/)
  for the trade-off discussion.
- Use a registry that supports **immutable tags + a "validated"
  flag** (Harbor, JFrog Artifactory) and switch the flag only
  after scan + sign succeed. Consumers ignore unvalidated tags.

Neither is documented in this recipe today. Open an issue if you
need one.

## Common pitfalls

- **`docker: true` is privileged-ish**: the agent's docker.sock
  IS root in the host. Don't expose this to untrusted pipelines.
  Project secrets keep registry creds masked, but the build
  context itself is what runs.
- **Multi-arch on x86 agents**: buildx with QEMU emulation works
  but is slow on arm64 (~3-5× x86 build time). If you ship
  multi-arch frequently, dedicate an arm64 agent in the runner
  pool — agent `tags:` can route arm64 builds to it.
- **Trivy on a private registry needs auth**: the build job's
  `docker login` doesn't survive into the trivy job (each job
  runs in a fresh container). The recipe above passes
  `username:`/`password:` to the trivy plugin which promotes
  them to `TRIVY_USERNAME`/`TRIVY_PASSWORD` env. For pre-build
  scans of FROM references — where there's no published image
  yet — use `scan-type: fs` against the Dockerfile dir instead;
  no registry auth needed.
- **Cosign + signed manifests + push order**: cosign signs the
  manifest IN the registry. The recipe above publishes first via
  `push: "true"` (multi-arch requires it) so the manifest exists
  before cosign runs. The signature anchors to the digest the
  tag points at, not the tag itself — so a future tag reuse
  doesn't invalidate the signature on the original digest.
- **Failed scan after publish needs cleanup**: when trivy fails,
  the image is already in the registry under the failed tag.
  Document a runbook step that deletes the tag (ghcr.io:
  `gh api -X DELETE /user/packages/container/myapp/versions/<id>`
  or via UI). Keeping the failed tag around isn't a security
  hole (cosign-sign never ran, so no signature claims it's
  trusted) but is operational hygiene.
