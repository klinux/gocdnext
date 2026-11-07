# gocdnext/docker-push — login + retag + push WITHOUT building.
#
# The complementary half of `gocdnext/docker`: this plugin
# assumes the image is already in the daemon (built by a
# previous step, or pulled from a source registry) and just
# tags it against another ref + pushes. Useful for:
#   - fan-out fan-in releases: one build, N taggers push to N
#     registries in parallel.
#   - promotion flows: pull from staging registry, tag for prod,
#     push to prod registry.
#
# Requires `docker: true` on the job so the daemon socket / DinD
# sidecar is wired in.
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_SOURCE    (required)  the image ref already in the
#                                daemon. Example:
#                                  "app:latest"
#                                  "ghcr.io/org/app@sha256:…"
#   PLUGIN_TARGET    (required)  the repo to push to, without a
#                                tag. Example: "ghcr.io/org/app".
#   PLUGIN_TAGS      (optional)  comma/space-separated tag list.
#                                Default "latest".
#   PLUGIN_PULL      (optional)  "true" → `docker pull` SOURCE
#                                first (useful when the source
#                                is in a different registry).
#                                Default "false".
#   PLUGIN_REGISTRY  (optional)  login host. Default: prefix of
#                                PLUGIN_TARGET up to the first "/".
#   PLUGIN_USERNAME  (optional)  registry user. Empty = no login.
#   PLUGIN_PASSWORD  (optional)  token/password via secrets.
# Base pinned by version+digest — shares gocdnext/docker's base. docker:24-cli
# still shipped fixable OS CRITICALs (Trivy --ignore-unfixed); 29.9.3-cli +
# `apk upgrade` clears them (0 fixable CRITICAL; residual HIGH in CHANGELOG).
# This plugin only tags + pushes, so the 29 CLI drives the runtime's default
# docker:24-dind daemon purely over the daemon API (proven in smoke_test.sh).
#
# NOTE: `apk upgrade` pulls from Alpine's MUTABLE repos — the FROM digest pins
# the base layer, not the upgraded package set. Reproducible base + current
# OS security patches.
FROM docker:29.9.3-cli@sha256:2357639ad1ea7d0194134bf3292622fd4e562483966ed674e5d7c945b5f3b28d

RUN apk upgrade --no-cache && apk add --no-cache bash

COPY entrypoint.sh /usr/local/bin/gocdnext-docker-push
RUN chmod +x /usr/local/bin/gocdnext-docker-push

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-docker-push"]
