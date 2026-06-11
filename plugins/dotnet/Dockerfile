# gocdnext/dotnet — .NET SDK toolchain plugin for gocdnext pipelines.
#
# Contract:
#   PLUGIN_COMMAND      (required)  dotnet subcommand + args, word-split.
#                                   Examples: "test -c Release", "build",
#                                   "publish -c Release -o out".
#   PLUGIN_WORKING_DIR  (optional)  path under /workspace; cd'd before
#                                   running dotnet. Defaults to the repo root.
#   PLUGIN_SDK          (optional)  "8" or "10" — pins the SDK major for
#                                   repos WITHOUT a global.json. Repos that
#                                   ship a global.json need nothing: the
#                                   dotnet muxer resolves it natively.
#
# Multi-SDK single image, same pattern as jdk-base: the 10.0 (current
# LTS) image is the base — its host/fxr muxer is the newest and runs
# every older runtime — and the 8.0 LTS SDK + shared frameworks are
# COPY'd in side-by-side. .NET SDKs are designed to coexist under one
# /usr/share/dotnet root; selection happens via global.json, so a repo
# pinning `"version": "8.0.x"` just works with zero plugin config.
#
# .NET 9 is intentionally absent by POLICY, not by support status:
# this image ships LTS SDKs only, mirroring jdk-base (11/17/21/25 —
# all LTS). 9 is STS and still in support until 2026-11-10, but STS
# churns the matrix every release for a sliver of demand; teams on
# an STS target can use plain `script:` with the official sdk image.
# When 8.0 leaves support (2026-11) drop the sdk8 stage and this
# becomes a plain 10.0 image until the next LTS lands.
FROM mcr.microsoft.com/dotnet/sdk:8.0 AS sdk8

FROM mcr.microsoft.com/dotnet/sdk:10.0

# Merge the 8.0 SDK + runtimes into the 10.0 root. COPY merges
# directories, so 8.0.x lands alongside 10.0.x without clobbering.
# host/fxr is deliberately NOT copied — the base image's (newer)
# muxer must win, and fxr is backward-compatible by design.
#
# packs/ is load-bearing, not optional: it carries the net8.0
# targeting/ref packs (Microsoft.NETCore.App.Ref, AspNetCore.App.Ref,
# host packs). Without it the SDK "recovers" by downloading them
# from NuGet on first build — which silently turns the "SDK 8
# included" promise into a network/feed dependency and defeats the
# offline-after-restore property the cache: block relies on.
# sdk-manifests/ + metadata/ (workload resolution for the 8.0
# feature band) and templates/ (`dotnet new` on SDK 8) ride along
# for the same reason: present locally or fetched at runtime.
COPY --from=sdk8 /usr/share/dotnet/sdk /usr/share/dotnet/sdk
COPY --from=sdk8 /usr/share/dotnet/shared /usr/share/dotnet/shared
COPY --from=sdk8 /usr/share/dotnet/packs /usr/share/dotnet/packs
COPY --from=sdk8 /usr/share/dotnet/sdk-manifests /usr/share/dotnet/sdk-manifests
COPY --from=sdk8 /usr/share/dotnet/templates /usr/share/dotnet/templates
COPY --from=sdk8 /usr/share/dotnet/metadata /usr/share/dotnet/metadata

# git for SourceLink + projects whose build reads VCS metadata;
# the SDK image ships ca-certificates + ICU already.
RUN apt-get update \
    && apt-get install -y --no-install-recommends git \
    && rm -rf /var/lib/apt/lists/*

# CI hygiene defaults. Telemetry opt-out is non-negotiable for a
# CI image; NOLOGO + SKIP_FIRST_TIME keep the first `dotnet` call
# from spending seconds printing banners and priming caches that
# die with the container anyway.
ENV DOTNET_CLI_TELEMETRY_OPTOUT=1 \
    DOTNET_NOLOGO=1 \
    DOTNET_SKIP_FIRST_TIME_EXPERIENCE=1

COPY entrypoint.sh /usr/local/bin/gocdnext-dotnet
RUN chmod +x /usr/local/bin/gocdnext-dotnet

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-dotnet"]
