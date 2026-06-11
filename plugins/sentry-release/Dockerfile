# gocdnext/sentry-release — create/finalize a Sentry release and
# optionally upload sourcemaps.
#
# Contract:
#   SENTRY_AUTH_TOKEN  (required)  via `secrets:` — sentry-cli
#                                  reads it natively from env.
#   SENTRY_ORG / SENTRY_PROJECT    via `secrets:`/variables or the
#                                  org:/project: inputs.
#   PLUGIN_VERSION     (optional)  default CI_TAG_NAME, then
#                                  CI_COMMIT_SHA.
#   PLUGIN_SOURCEMAPS  (optional)  dir to upload as sourcemaps.
#   PLUGIN_FINALIZE    (optional)  default true.
#   PLUGIN_SET_COMMITS (optional)  --commit metadata via --auto.
FROM getsentry/sentry-cli:2

COPY entrypoint.sh /usr/local/bin/gocdnext-sentry-release
USER root
RUN chmod +x /usr/local/bin/gocdnext-sentry-release

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-sentry-release"]
