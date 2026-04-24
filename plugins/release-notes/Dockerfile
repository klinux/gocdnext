# gocdnext/release-notes — generate a changelog between two git
# refs and write it to a file under the workspace. The file is
# meant to be consumed downstream by gocdnext/github-release (as
# the --notes-file body), gocdnext/email (as the message body),
# or any other task that reads a plain text.
#
# Two output formats:
#   - plain (default): "- <short sha> <subject> (<author>)" per line.
#     Readable, no dependency on a commit convention.
#   - conventional: groups commits under "Features", "Bug Fixes",
#     "Chores", "Other" headings by parsing Conventional Commits
#     prefixes (feat:/fix:/chore:/etc.). Quiet fallback to the
#     plain shape when a repo isn't following the convention —
#     no commits get dropped, just labelled "Other".
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_FROM      (optional)  starting ref (exclusive). Defaults
#                                to the last reachable tag via
#                                `git describe --tags --abbrev=0`,
#                                falling back to the root commit
#                                when no tag exists.
#   PLUGIN_TO        (optional)  ending ref (inclusive). Defaults
#                                to HEAD.
#   PLUGIN_OUTPUT    (optional)  workspace-relative output file.
#                                Default "RELEASE_NOTES.md".
#   PLUGIN_FORMAT    (optional)  "plain" (default) | "conventional"
#   PLUGIN_HEADING   (optional)  first-line heading to write above
#                                the body, e.g. "## v1.2.3". Empty
#                                = no heading; the body starts
#                                straight at the first bullet.
FROM alpine:3.20

RUN apk add --no-cache bash git ca-certificates

COPY entrypoint.sh /usr/local/bin/gocdnext-release-notes
RUN chmod +x /usr/local/bin/gocdnext-release-notes

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-release-notes"]
