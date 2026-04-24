# gocdnext/trivy — scan images, filesystems, or repos for CVEs +
# secret leaks. Wraps Aqua's official CLI.
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_SCAN_TYPE       (optional)  image | fs | repo | config.
#                                      Defaults to "fs" (scans the workspace).
#   PLUGIN_TARGET          (optional)  what to scan:
#                                        image: "ghcr.io/org/app:v1"
#                                        fs:    "./" (defaults to "/workspace")
#                                        repo:  "https://github.com/org/repo"
#                                        config: "./k8s/"
#                                      Defaults differ per scan_type.
#   PLUGIN_SEVERITY        (optional)  comma-separated severity filter:
#                                      LOW,MEDIUM,HIGH,CRITICAL.
#                                      Defaults to HIGH,CRITICAL so CI
#                                      isn't drowned in LOW noise.
#   PLUGIN_EXIT_CODE       (optional)  exit code when any finding matches the
#                                      severity filter. Default 1 (fail CI).
#                                      Set to 0 for advisory-only scans.
#   PLUGIN_IGNORE_UNFIXED  (optional)  "true" to skip CVEs without a fix
#                                      available. Default "true" — reduces
#                                      noise on libraries the upstream
#                                      hasn't patched yet.
#   PLUGIN_FORMAT          (optional)  table | json | sarif | template.
#                                      Default table for human logs.
#   PLUGIN_OUTPUT          (optional)  write report to this path (relative
#                                      to workspace). Empty = stdout only.
FROM aquasec/trivy:0.56.2

RUN apk add --no-cache bash

COPY entrypoint.sh /usr/local/bin/gocdnext-trivy
RUN chmod +x /usr/local/bin/gocdnext-trivy

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-trivy"]
