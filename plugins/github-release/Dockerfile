# gocdnext/github-release — publish a release to GitHub. Wraps
# the `gh` CLI so operators don't hand-craft curl calls against
# the REST API. Creates the release, optionally generates notes
# from commits since the last tag, and uploads asset files.
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_TAG          (required)  git tag for the release,
#                                   e.g. "v1.2.3".
#   PLUGIN_REPO         (optional)  "owner/name". Defaults to
#                                   inferring from the workspace
#                                   git remote via `gh repo view`.
#   PLUGIN_TITLE        (optional)  release title. Defaults to
#                                   the tag name.
#   PLUGIN_NOTES        (optional)  body text for the release.
#                                   Empty + PLUGIN_GENERATE_NOTES
#                                   true = gh auto-generates from
#                                   commit history.
#   PLUGIN_GENERATE_NOTES (optional) "true"/"false"; default
#                                   "true" when PLUGIN_NOTES empty.
#   PLUGIN_DRAFT        (optional)  "true" creates the release as
#                                   a draft (visible only to repo
#                                   admins until published).
#   PLUGIN_PRERELEASE   (optional)  "true" marks as pre-release.
#   PLUGIN_ASSETS       (optional)  newline- or comma-separated
#                                   list of workspace-relative
#                                   paths to upload as assets.
#                                   Globs supported (gh expands).
#   PLUGIN_TOKEN        (required)  GitHub token with `repo`
#                                   scope. Pipe from the job's
#                                   `secrets:` list.
#
# `gh` authenticates via GH_TOKEN env — we set it from
# PLUGIN_TOKEN so credentials never hit the command line.
FROM alpine:3.20

RUN apk add --no-cache bash git github-cli ca-certificates

COPY entrypoint.sh /usr/local/bin/gocdnext-github-release
RUN chmod +x /usr/local/bin/gocdnext-github-release

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-github-release"]
