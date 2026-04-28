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
FROM golangci/golangci-lint:v1.63

COPY entrypoint.sh /usr/local/bin/gocdnext-golangci-lint
RUN chmod +x /usr/local/bin/gocdnext-golangci-lint

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-golangci-lint"]
