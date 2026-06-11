# gocdnext/gh-pages — publish a built directory to a Git branch
# (GitHub Pages convention, works on any host).
#
# Contract:
#   GIT_TOKEN          (required)  push token via `secrets:` —
#                                  injected into the push URL in
#                                  memory only, never argv/logs.
#   PLUGIN_DIR         (required)  the built site directory.
#   PLUGIN_BRANCH      (optional)  default gh-pages.
#   PLUGIN_REMOTE_URL  (optional)  default: derived from the
#                                  workspace clone's origin.
#   PLUGIN_CNAME       (optional)  writes a CNAME file.
#
# Strategy: fresh orphan commit per deploy, force-pushed. Pages
# branches are generated artifacts — their history is noise, and
# an ever-growing branch of minified bundles bloats every future
# clone of the repo.
FROM alpine:3.21

RUN apk add --no-cache git

COPY entrypoint.sh /usr/local/bin/gocdnext-gh-pages
RUN chmod +x /usr/local/bin/gocdnext-gh-pages

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-gh-pages"]
