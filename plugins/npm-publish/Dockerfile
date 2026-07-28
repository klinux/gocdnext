# gocdnext/npm-publish — publish a package to the npm registry
# (or a compatible private one).
#
# Contract:
#   NPM_TOKEN          (token auth) via the job's `secrets:` — written
#                                  to a user-scoped .npmrc, never argv.
#   PLUGIN_AUTH        (optional)  auto | token | basic (default auto).
#   PLUGIN_USERNAME    (basic auth) private-registry username (Nexus/
#                                  Artifactory). Pair with PLUGIN_PASSWORD.
#   PLUGIN_PASSWORD    (basic auth) base64'd with the username into the
#                                  user-scoped .npmrc `_auth`, never argv.
#   PLUGIN_DIR         (optional)  package dir, default ".".
#   PLUGIN_TAG         (optional)  dist-tag, default "latest".
#   PLUGIN_ACCESS      (optional)  public | restricted.
#   PLUGIN_REGISTRY    (optional)  registry URL, default npmjs.
#   PLUGIN_IF_EXISTS   (optional)  fail | skip — what to do when
#                                  name@version is already published
#                                  (idempotent retries, same
#                                  philosophy as artifact dedupe).
#   PLUGIN_DRY_RUN     (optional)  npm publish --dry-run.
FROM node:22-alpine

RUN apk add --no-cache jq

COPY entrypoint.sh /usr/local/bin/gocdnext-npm-publish
RUN chmod +x /usr/local/bin/gocdnext-npm-publish

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-npm-publish"]
