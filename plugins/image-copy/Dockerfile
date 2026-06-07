# gocdnext/image-copy — promote a published container image (or
# multi-arch manifest list) from one registry to another, preserving
# the full manifest tree.
#
# Why this exists separate from gocdnext/docker-push:
#
# docker-push does `docker tag` + `docker push` on the agent's local
# daemon. For multi-arch images that flow LOSES the manifest list —
# the local daemon stores a single platform manifest and re-pushes
# that one, breaking `docker pull` on the "wrong" architecture for
# anyone downstream. image-copy uses registry-API copy primitives
# that preserve the index, so a `staging-registry/foo:vX → prod-
# registry/foo:vX` promotion stays multi-arch end-to-end.
#
# Three interchangeable backends:
#   - crane   (default) — single static binary from go-containerregistry.
#                         Fast, multi-arch native, no shared state.
#   - skopeo            — broader OCI Swiss-army knife. Same multi-
#                         arch shape (`--multi-arch all`), wider
#                         output-format support if a follow-up step
#                         needs OCI archive / dir / etc.
#   - buildx-imagetools — `docker buildx imagetools create --tag DST SRC`.
#                         Requires the job to have `docker: true` so
#                         the buildkit/docker daemon is reachable.
#                         Pick this when your build pipeline already
#                         needs the docker socket and you don't want
#                         to add another binary.
#
# NONE of the three transfer cosign signatures / attestations
# today. Those live as separate registry artifacts (the
# `sha256-X.sig` triangulation tags) and require either `cosign
# copy` (roadmap: future `cosign-copy` backend here) or a re-sign
# at the target. Re-sign anchored to the PROMOTED_DIGEST the
# plugin emits is the recommended pattern — closes the "what got
# signed?" race because everything attests one digest end-to-end.
#
# Always emits the promoted digest via crane (the smallest, most
# reliable digest tool of the three) into the workspace output file
# so a downstream cosign-sign job can anchor by digest rather than
# by mutable tag.
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_SOURCE          (required)  source image ref. Either a
#                                      tag (`registry/repo:tag`) or
#                                      a digest pin
#                                      (`registry/repo@sha256:...`).
#                                      Digest pin recommended for
#                                      promotion — no race with a
#                                      retag of the staging image
#                                      between scan and promote.
#   PLUGIN_TARGET          (required)  target image ref:
#                                      `registry/repo:tag`. One
#                                      destination per job; use
#                                      `extra-tags:` for additional
#                                      tags pointing at the same
#                                      digest in the SAME repo.
#   PLUGIN_EXTRA_TAGS      (optional)  newline-separated additional
#                                      tags applied to the same
#                                      target repo (e.g. `latest`,
#                                      `v1`, `v1.2`). Each tag goes
#                                      through the chosen backend's
#                                      cheap retag path — for crane
#                                      it's `crane tag DST <name>`,
#                                      for skopeo a second copy from
#                                      target back to target.
#   PLUGIN_BACKEND         (optional)  "crane" (default) | "skopeo" |
#                                      "buildx-imagetools".
#   PLUGIN_USERNAME        (optional)  registry user for the TARGET.
#                                      Promoted to the backend's
#                                      auth file (never argv).
#                                      Sourced via `secrets:`.
#   PLUGIN_PASSWORD        (optional)  registry password / token for
#                                      the TARGET. Sourced via
#                                      `secrets:`.
#   PLUGIN_SOURCE_USERNAME (optional)  registry user for the SOURCE.
#                                      Defaults to PLUGIN_USERNAME
#                                      ONLY when source + target
#                                      share the host (same-registry
#                                      promotion). On cross-registry
#                                      the target token is NOT
#                                      forwarded to the source — a
#                                      missing source cred leaves
#                                      the source anonymous, which
#                                      is correct for public mirrors
#                                      and refuses silent cred
#                                      leak to a stranger.
#   PLUGIN_SOURCE_PASSWORD (optional)  source password / token.
#                                      Same defaulting rule as
#                                      PLUGIN_SOURCE_USERNAME.
#   PLUGIN_OUTPUT          (optional)  workspace-relative output
#                                      file. Default
#                                      ".gocdnext/image-copy.env".
#                                      Shell-sourceable: PROMOTED_DIGEST=,
#                                      SOURCE=, TARGET=, BACKEND=.
#
# Exit codes:
#   0 — copy succeeded, output file written.
#   2 — input validation error (missing source/target, unknown
#       backend, bad charset on a ref/tag).
#   3 — backend invocation failed (network, auth, missing source
#       manifest, registry refused write).

# crane is published from go-containerregistry as a distroless
# image; upstream only ships `:latest` + `:debug` tags (no semver
# tags). Pin by digest for a reproducible build — bump the digest
# when intentionally adopting a newer crane.
FROM gcr.io/go-containerregistry/crane@sha256:0ae17ecb34315aa7cbff28f6eddee3b7adae0b2f90101260d990804db1eb0084 AS crane

FROM alpine:3.20

# skopeo: from alpine main since 3.13. jq: parsing digests / manifest
# JSON from skopeo inspect output. bash: %-prefixed shell expansion
# semantics rely on bash, ash would silently surprise on edge cases.
# coreutils: GNU base64 (-w0 flag) for the docker config.json auth
# encoding — BusyBox base64 doesn't support `-w` and wraps at 76
# chars, which breaks the docker config json string field.
# docker-cli + docker-cli-buildx: required ONLY by the
# buildx-imagetools backend (the job still needs `docker: true` so
# the agent mounts the host docker socket, but `docker` itself
# must be IN the plugin image — `docker: true` only exposes the
# daemon, not the CLI binary). Crane / skopeo paths don't touch
# these binaries. ~85MB added; acceptable for an opt-in backend
# whose alternative is "deploy a second plugin".
RUN apk add --no-cache bash ca-certificates skopeo jq coreutils docker-cli docker-cli-buildx

COPY --from=crane /ko-app/crane /usr/local/bin/crane

COPY entrypoint.sh /usr/local/bin/gocdnext-image-copy
RUN chmod +x /usr/local/bin/gocdnext-image-copy

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-image-copy"]
