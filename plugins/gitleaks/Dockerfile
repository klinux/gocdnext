# gocdnext/gitleaks — secret scanner. Runs against the workspace
# checkout looking for accidentally-committed credentials,
# tokens, and keys. Cheap to add at the front of any pipeline
# and catches the mistake before the commit propagates further.
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_PATH       (optional)  path under /workspace to scan.
#                                 Defaults to ".".
#   PLUGIN_CONFIG     (optional)  path to a gitleaks.toml config
#                                 (relative to the workspace).
#                                 Empty = gitleaks default rules.
#   PLUGIN_FORMAT     (optional)  json | sarif | csv. Default
#                                 "json" (trivial to grep).
#   PLUGIN_REPORT     (optional)  path (under workspace) to write
#                                 the report to. Pair with
#                                 artifacts.optional to attach.
#   PLUGIN_SCAN_MODE  (optional)  "dir" | "git". "dir" scans the
#                                 working tree; "git" scans the
#                                 full git history. Default "dir"
#                                 — most CI runs care about the
#                                 current revision, not
#                                 regressions buried in history.
#   PLUGIN_EXIT_CODE  (optional)  exit code when leaks are found.
#                                 Default 1 (fail CI). Set to 0
#                                 for advisory-only scans.
FROM zricethezav/gitleaks:v8.21.2

RUN apk add --no-cache bash git

COPY entrypoint.sh /usr/local/bin/gocdnext-gitleaks
RUN chmod +x /usr/local/bin/gocdnext-gitleaks

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-gitleaks"]
