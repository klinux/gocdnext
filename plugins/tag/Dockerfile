# gocdnext/tag — create a git tag on the current commit and push
# it to the origin remote. The canonical "publish a release tag
# from CI" step — keeps the release workflow entirely in the
# pipeline (no manual `git tag` on a laptop).
#
# Auth: the plugin authenticates the push via an HTTPS token
# (GitHub PAT / GitLab job token / Bitbucket app password).
# SSH-based auth is deliberately NOT supported in v1 — it needs
# a persisted SSH identity the agent would have to juggle; keep
# it simple and push over HTTPS. Operators with an SSH-only
# remote drop down to script + git.
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_NAME      (required)  tag name, e.g. "v1.2.3".
#   PLUGIN_MESSAGE   (optional)  annotated-tag message. When set,
#                                the tag is created with -a -m.
#                                When empty, a lightweight tag
#                                (no message) is pushed instead —
#                                which is fine for CI-internal
#                                markers but meaningless for
#                                release tooling that expects an
#                                annotation.
#   PLUGIN_REVISION  (optional)  commit SHA to tag. Defaults to
#                                HEAD, which is almost always what
#                                you want (tag the commit the
#                                pipeline's running against).
#   PLUGIN_REMOTE    (optional)  git remote name. Default "origin".
#   PLUGIN_USERNAME  (optional)  HTTPS auth user (GitHub: literal
#                                "x-access-token"; GitLab: "oauth2";
#                                Bitbucket: the account name).
#                                Defaults to "git" which most
#                                providers ignore next to a token.
#   PLUGIN_TOKEN     (required)  HTTPS auth token. Pipe via
#                                secrets: — it's the only secret
#                                on the wire.
#   PLUGIN_FORCE     (optional)  "true" pushes with --force, useful
#                                for moving a "latest" tag. Default
#                                "false"; never re-tag a release
#                                version by accident.
FROM alpine:3.20

RUN apk add --no-cache bash git ca-certificates

COPY entrypoint.sh /usr/local/bin/gocdnext-tag
RUN chmod +x /usr/local/bin/gocdnext-tag

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-tag"]
