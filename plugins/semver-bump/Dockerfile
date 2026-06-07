# gocdnext/semver-bump — compute the next SemVer version from the
# Conventional Commits since the prior tag.
#
# Output: a shell-sourceable file under the workspace with the
# computed values. Downstream jobs `source` it and reference
# $NEXT in their `git tag` / build / publish steps. This matches
# the gocdnext catalog idiom: plugins emit files, jobs read
# them via the shared workspace + (in isolated runtimes) via
# artifacts.paths + needs_artifacts.
#
# Pairs naturally with gocdnext/release-notes (same conventional-
# commit scan) and the trunk-based-release recipe — release.yaml
# becomes "semver-bump → release-notes → create-tag → push",
# with no operator-typed TAG variable on the trigger.
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_PRIOR_TAG   (optional)  override for the prior-tag
#                                  detection. Must itself be a
#                                  SemVer-shaped tag (e.g.
#                                  `v1.2.3`); used verbatim as
#                                  the baseline whose version is
#                                  parsed and bumped. Defaults to
#                                  the nearest reachable matching
#                                  tag via `git describe --tags
#                                  --match '<prefix>[0-9]*'
#                                  --abbrev=0` — the `[0-9]*`
#                                  filter rejects non-SemVer
#                                  siblings like `vfoo` / `vnext`
#                                  that share the prefix. When no
#                                  prior tag exists, INITIAL is
#                                  used as the baseline.
#   PLUGIN_PREFIX      (optional)  tag prefix. Default "v" — so a
#                                  repo tagged v1.2.3 / v1.2.4 …
#                                  is read by `--match 'v*'`. Set
#                                  empty for SemVer-only tags
#                                  (1.2.3 style); use a custom
#                                  prefix (e.g. "release-") to
#                                  scope to a sub-track.
#   PLUGIN_INITIAL     (optional)  first version when no prior
#                                  tag exists. Default "0.1.0"
#                                  (the SemVer-recommended start
#                                  for libraries; bump-major
#                                  detection still applies on top
#                                  of this).
#   PLUGIN_PRE_RELEASE (optional)  pre-release suffix appended to
#                                  the computed next version, e.g.
#                                  "rc1" → v1.3.0-rc1. Empty
#                                  (default) emits a stable tag.
#   PLUGIN_FORCE_KIND  (optional)  override the auto-detected
#                                  bump kind. One of
#                                  major | minor | patch. Empty
#                                  (default) uses Conventional
#                                  Commits scan. Useful when the
#                                  operator KNOWS the next release
#                                  is a major even if the commits
#                                  read as patch (e.g. a doc-only
#                                  cycle that ships a deprecated-
#                                  API removal flagged separately).
#   PLUGIN_OUTPUT      (optional)  workspace-relative output file.
#                                  Default ".gocdnext/semver.env".
#                                  Created with `mkdir -p`. Shell-
#                                  sourceable: NEXT=, CURRENT=,
#                                  KIND=, PREV_SHA= lines.
#
# Exit codes:
#   0 — wrote the output file successfully (even on no-commits-
#        since-last-tag, which emits KIND=none + NEXT=CURRENT so
#        downstream can branch on it without parsing logs).
#   2 — input validation error (unknown force-kind value,
#        malformed initial version, etc.). Run fails loud.
#   3 — git command failure (not a git repo, etc.). Run fails.
FROM alpine:3.20

RUN apk add --no-cache bash git ca-certificates

COPY entrypoint.sh /usr/local/bin/gocdnext-semver-bump
RUN chmod +x /usr/local/bin/gocdnext-semver-bump

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-semver-bump"]
