# gocdnext/coveralls — upload a coverage report to Coveralls.
# Uses the Go-based `coveralls-cli` (kofer-san/coveralls-go-cli
# fork of the original node tooling; reliable + no npm). Accepts
# LCOV, JSON, or generic Coveralls "simplecov" format — the
# common interchange formats all coverage tools export to.
#
# Auth: a repo token (Coveralls cloud) or the per-repo token
# from Coveralls Enterprise. Pipe via secrets:.
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_TOKEN        (required)  repo token.
#   PLUGIN_FILE         (optional)  coverage file. Default
#                                   "coverage/lcov.info" —
#                                   convention for npm/jest/node.
#   PLUGIN_FORMAT       (optional)  "lcov" (default) | "json" |
#                                   "simplecov" | "cobertura".
#   PLUGIN_WORKING_DIR  (optional)  cd here first. Default ".".
#   PLUGIN_PARALLEL     (optional)  "true" tags the upload as one
#                                   parallel slice. Coveralls
#                                   merges parallel jobs on the
#                                   commit after a finalise call.
#   PLUGIN_FINALISE     (optional)  "true" marks the build done —
#                                   call once AFTER all parallel
#                                   slices have uploaded.
#   PLUGIN_BASE_URL     (optional)  Coveralls Enterprise URL.
FROM node:20-alpine

RUN apk add --no-cache bash ca-certificates git \
    && npm install -g coveralls-next@^5.0.0

COPY entrypoint.sh /usr/local/bin/gocdnext-coveralls
RUN chmod +x /usr/local/bin/gocdnext-coveralls

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-coveralls"]
