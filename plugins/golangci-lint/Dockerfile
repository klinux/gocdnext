# gocdnext/golangci-lint — run golangci-lint against a Go module.
# Wraps the upstream alpine image so pipelines don't have to pin
# the linter version per-job.
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
# golangci-lint v2.x, built with Go 1.26. The build-time Go must be >= the
# `go` directive of the module being linted (upstream compat rule); the old
# v1.63 image (Go 1.23) hard-refused Go 1.26 modules, and the v1 line is EOL at
# Go 1.24, so there is no v1 build that can lint Go 1.26. v2 also changes the
# `.golangci.yml` schema (needs `version: "2"`) — see plugin.yaml.
FROM golangci/golangci-lint:v2.9.0

COPY entrypoint.sh /usr/local/bin/gocdnext-golangci-lint
RUN chmod +x /usr/local/bin/gocdnext-golangci-lint

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-golangci-lint"]
