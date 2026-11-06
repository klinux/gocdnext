# gocdnext/buildx — container build + push via `docker buildx`.
# Single-arch by default (linux/amd64); declare `platforms:` in
# with: for multi-arch and the plugin sets up QEMU binfmt only
# when the target includes a non-host architecture.
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_IMAGE      (required)  target repo, e.g. "ghcr.io/org/app".
#   PLUGIN_TAGS       (optional)  comma/space-separated tags. Default "latest".
#   PLUGIN_PLATFORMS  (optional)  comma-separated. Default
#                                 "linux/amd64". Add linux/arm64,
#                                 linux/arm/v7, etc. to opt into
#                                 multi-arch — the plugin then
#                                 registers QEMU handlers on demand.
#   PLUGIN_DOCKERFILE (optional)  default "Dockerfile".
#   PLUGIN_CONTEXT    (optional)  default ".".
#   PLUGIN_BUILD_ARGS (optional)  KEY=VALUE, comma- or newline-sep.
#   PLUGIN_PUSH       (optional)  "true"/"false". Default "true".
#                                 "false" builds the cross images
#                                 into the builder cache but does
#                                 NOT pull them into the local
#                                 daemon — multi-arch manifests
#                                 can't live in the legacy store.
#   PLUGIN_REGISTRY   (optional)  login host. Default: prefix of
#                                 PLUGIN_IMAGE up to the first "/".
#   PLUGIN_USERNAME   (optional)  registry user. Empty = skip login.
#   PLUGIN_PASSWORD   (optional)  piped from secrets:.
#   PLUGIN_BUILDKIT_IMAGE (optional)  override the BuildKit container
#                                 image used by `docker buildx
#                                 create`. Defaults to a version+digest
#                                 PINNED moby/buildkit in entrypoint.sh
#                                 (NOT docker's floating
#                                 `buildx-stable-1`) so the builder is
#                                 reproducible and auditable. The pinned
#                                 default is new enough to honour the
#                                 aws-sdk-go-v2 auto-checksum opt-out the
#                                 non-AWS S3 cache path needs (see Layer
#                                 cache below).
#
# Layer cache (PLUGIN_CACHE shorthand + env from runner profile):
#   PLUGIN_CACHE                  registry | inline | bucket | none
#   PLUGIN_CACHE_TO / _FROM       explicit override, wins when set
#   GOCDNEXT_LAYER_CACHE_BACKEND  s3 | gcs (gcs is sugar for s3 +
#                                 storage.googleapis.com endpoint).
#                                 Default s3.
#   GOCDNEXT_LAYER_CACHE_BUCKET   bucket name (required for `bucket`).
#   GOCDNEXT_LAYER_CACHE_REGION   default us-east-1 (s3) / auto (gcs).
#   GOCDNEXT_LAYER_CACHE_ENDPOINT explicit endpoint URL; auto-set for gcs.
#   GOCDNEXT_LAYER_CACHE_NAME     cache key, default = PLUGIN_IMAGE.
#   AWS_ACCESS_KEY_ID + AWS_SECRET_ACCESS_KEY  credentials (BuildKit's
#                                 s3 backend reads these regardless of
#                                 the actual provider — for GCS use
#                                 HMAC interop keys).
#
# Requires `docker: true` on the job. The bundled image ships
# buildx + binfmt setup so the job can `exec docker buildx`
# against the daemon the agent wired in.
# Base pinned by version+digest. docker:24-cli still shipped fixable OS
# CRITICALs (Trivy --ignore-unfixed); 29.9.3-cli + `apk upgrade` clears them
# (0 fixable CRITICAL; residual HIGH classified in CHANGELOG). The 29 CLI drives
# the runtime's default docker:24-dind daemon fine (proven: a real multi-arch
# amd64+arm64 build + registry cache round-trip in smoke_test.sh). buildx spawns
# a standalone BuildKit builder, pinned by digest in entrypoint.sh (not docker's
# floating buildx-stable-1).
#
# The buildx CLI plugin itself is already BUNDLED in this base
# (/usr/local/libexec/docker/cli-plugins/docker-buildx, v0.38.2) — the one
# `docker buildx` actually resolves to — so we do NOT `apk add docker-cli-buildx`
# (an older, ~71 MiB, never-executed duplicate in /usr/libexec).
#
# NOTE: `apk upgrade` pulls from Alpine's MUTABLE repos — the FROM digest pins
# the base layer, not the upgraded package set. Reproducible base + current
# OS security patches.
FROM docker:29.9.3-cli@sha256:2357639ad1ea7d0194134bf3292622fd4e562483966ed674e5d7c945b5f3b28d

RUN apk upgrade --no-cache && apk add --no-cache bash

COPY entrypoint.sh /usr/local/bin/gocdnext-buildx
RUN chmod +x /usr/local/bin/gocdnext-buildx

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-buildx"]
