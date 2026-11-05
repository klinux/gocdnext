# gocdnext/golangci-lint — run golangci-lint against a Go module.
# Wraps the linter so pipelines don't have to pin the version per-job.
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_ARGS         (optional)  extra args appended after `run`.
#                                   Example: "--timeout 5m ./..."
#                                   Defaults to "./..." (lint the
#                                   working-dir module).
#   PLUGIN_WORKING_DIR  (optional)  cd here first. Default ".".
#   PLUGIN_TIMEOUT      (optional)  passes through to --timeout.
#                                   Default "5m" — generated sqlc +
#                                   pgx types push past the 1m
#                                   default in larger workspaces.
#
# golangci-lint is built FROM SOURCE on a patched Go base, NOT pulled as the
# upstream pre-built image:
#   - The linter's build-time Go must be >= the linted module's `go` directive
#     (upstream compat rule). Go 1.27 lints modules up to go 1.27. The old
#     v1.63 image (Go 1.23) hard-refused Go 1.26 modules, and the v1 line is EOL
#     at Go 1.24 — there is no v1 build that can lint Go >= 1.26.
#   - The Go STDLIB is compiled into the linter, so its CVEs are only fixed by a
#     Go rebuild. Upstream's images lag Go patch releases and ship those stdlib
#     CVEs until their next build; building here on golang:1.27.3-alpine gives
#     0 HIGH/0 CRITICAL (Trivy --ignore-unfixed) today and makes the Go version
#     OUR knob — a stdlib fix is a one-line FROM bump (Dependabot-watched),
#     not a wait on upstream's release cadence.
#   - `go install` (not the upstream install script) is deliberate: the script
#     fetches upstream's PRE-BUILT binary, which is exactly the stale-stdlib
#     artefact we're avoiding. Compiling the pinned tag with our Go is the
#     point; it only costs the release commit/date stamp (cosmetic — `version`
#     still reports v2.15.0).
#
# golangci-lint v2 also changes the `.golangci.yml` schema (needs
# `version: "2"`) — see plugin.yaml.
FROM golang:1.27.3-alpine

# git: the entrypoint's dubious-ownership workaround + VCS-aware lint. ca-certs:
# module fetches over TLS. apk upgrade pulls OS packages patched since the base
# was cut. The build caches are dropped from the layer (the entrypoint points
# GOMODCACHE/GOCACHE at the workspace at run time, so they're dead weight here).
RUN apk add --no-cache git ca-certificates \
 && apk upgrade --no-cache \
 && go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0 \
 && rm -rf /go/pkg /root/.cache

COPY entrypoint.sh /usr/local/bin/gocdnext-golangci-lint
RUN chmod +x /usr/local/bin/gocdnext-golangci-lint

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-golangci-lint"]
