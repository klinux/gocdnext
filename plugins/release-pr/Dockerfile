# gocdnext/release-pr — open (or update) a curated release PR. Bumps
# a VERSION file on a release/<version> branch and opens a PR for peer
# sign-off; merging it (touching VERSION) is the signal a downstream
# tag pipeline keys on. Wraps git + the `gh` CLI so the release flow
# lives in one job instead of a hand-rolled script.
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_VERSION       (required)  release version, bare ("1.3.0")
#                                    or prefixed ("v1.3.0") — a leading
#                                    tag_prefix is stripped. Usually
#                                    from a semver-bump step.
#   PLUGIN_TOKEN         (required)  GitHub token (contents:write +
#                                    pull-requests:write). Drives both
#                                    the branch push (via GIT_ASKPASS,
#                                    clean URL) and gh (GH_TOKEN). From
#                                    secrets:.
#   PLUGIN_REPO          (optional)  "owner/name". gh infers from the
#                                    remote when omitted.
#   PLUGIN_BASE          (optional)  base branch (default "main").
#   PLUGIN_VERSION_FILE  (optional)  file to bump (default "VERSION");
#                                    keep == the tag pipeline's
#                                    when.paths.
#   PLUGIN_NOTES_FILE    (optional)  changelog file used as PR body.
#   PLUGIN_LABEL         (optional)  PR label (default "release");
#                                    must already exist in the repo.
#   PLUGIN_TITLE         (optional)  PR title (default "Release
#                                    <tag_prefix><version>").
#   PLUGIN_BRANCH_PREFIX (optional)  bot branch prefix (default
#                                    "release/").
#   PLUGIN_TAG_PREFIX    (optional)  version prefix (default "v").
#
# Outputs (to $GOCDNEXT_OUTPUT_FILE): pr_url, version, release_tag.
#
# Both gh (GH_TOKEN) and the git push (GIT_ASKPASS, clean URL) take
# the token from env — it never appears on argv or in a config file.
FROM alpine:3.20

RUN apk add --no-cache bash git github-cli ca-certificates

COPY entrypoint.sh /usr/local/bin/gocdnext-release-pr
RUN chmod +x /usr/local/bin/gocdnext-release-pr

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-release-pr"]
