# gocdnext/nexus — upload to + download from a Sonatype Nexus
# repository. Single plugin, `action: upload | download` toggles
# direction so pipelines can both publish their own artefacts
# AND pull a shared library version in the same family of jobs.
#
# Philosophy: cover the 90% Maven2 layout path (group/artifact/
# version/extension) with the Nexus REST API, lean on curl +
# jq for the rest. Operators with a raw repository or a custom
# layout drop down to `script:` + curl — not the job of this
# plugin to re-implement the full Nexus search surface.
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_ACTION       (required)  "upload" | "download"
#   PLUGIN_URL          (required)  Nexus base URL (no trailing
#                                   slash); e.g. https://nexus.corp
#   PLUGIN_REPOSITORY   (required)  repository name (hosted for
#                                   upload, hosted/group for
#                                   download).
#   PLUGIN_USERNAME     (optional)  basic-auth user; pipe via
#                                   secrets:.
#   PLUGIN_PASSWORD     (optional)  basic-auth password/token;
#                                   pipe via secrets:.
#
#   # Maven2 coordinates — used by both actions:
#   PLUGIN_GROUP_ID     (required)  e.g. "com.acme.shared".
#   PLUGIN_ARTIFACT_ID  (required)  e.g. "common-lib".
#   PLUGIN_EXTENSION    (optional)  default "jar".
#   PLUGIN_CLASSIFIER   (optional)  Maven classifier (sources,
#                                   javadoc, linux-x86_64, …).
#
#   # Download-only:
#   PLUGIN_VERSION      (required when action=download)  exact
#                       version OR "LATEST" to resolve newest
#                       via the REST search endpoint.
#   PLUGIN_DEST         (optional)  destination dir/path inside
#                       workspace. Default "." — the resolved
#                       file lands alongside the script's cwd.
#
#   # Upload-only:
#   PLUGIN_FILE         (required when action=upload)  workspace
#                       path of the artefact to publish.
#   PLUGIN_UPLOAD_VERSION (required when action=upload)  version
#                       label to publish under. Separated from
#                       PLUGIN_VERSION so download ("LATEST")
#                       and upload ("1.2.3") don't share the
#                       same knob.
FROM alpine:3.20

RUN apk add --no-cache bash ca-certificates curl jq

COPY entrypoint.sh /usr/local/bin/gocdnext-nexus
RUN chmod +x /usr/local/bin/gocdnext-nexus

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-nexus"]
