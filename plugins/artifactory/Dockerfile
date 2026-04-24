# gocdnext/artifactory — upload to + download from a JFrog
# Artifactory repository. Sibling of gocdnext/nexus with the
# same action: upload|download shape; the only difference is
# the REST endpoint used to resolve `version: LATEST`:
# Artifactory speaks AQL (Artifactory Query Language), Nexus
# speaks its /service/rest/v1/search/assets. Direct-coordinate
# GET/PUT against /artifactory/{repo}/{path} is identical
# across vendors — that's Maven2 layout, not JFrog-specific.
#
# Auth:
# - Basic (username + password/API key), OR
# - Bearer token (Access Token / Identity Token). Preferred
#   in modern deployments; one `token:` input covers both.
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_ACTION       (required)  "upload" | "download"
#   PLUGIN_URL          (required)  Artifactory base URL; e.g.
#                                   https://mycorp.jfrog.io OR
#                                   https://myart.corp/artifactory
#                                   — both accepted, the script
#                                   normalises the trailing
#                                   "/artifactory" segment.
#   PLUGIN_REPOSITORY   (required)  repo key.
#   PLUGIN_USERNAME     (optional)  basic-auth user.
#   PLUGIN_PASSWORD     (optional)  basic-auth password/API key.
#   PLUGIN_TOKEN        (optional)  Bearer token; wins over
#                                   username/password when set.
#
#   # Maven2 coordinates (both actions):
#   PLUGIN_GROUP_ID     (required)  e.g. "com.acme.shared".
#   PLUGIN_ARTIFACT_ID  (required)  e.g. "common-lib".
#   PLUGIN_EXTENSION    (optional)  default "jar".
#   PLUGIN_CLASSIFIER   (optional)  Maven classifier.
#
#   # Download-only:
#   PLUGIN_VERSION      exact OR "LATEST" (AQL search).
#   PLUGIN_DEST         default ".".
#
#   # Upload-only:
#   PLUGIN_FILE         workspace-relative source path.
#   PLUGIN_UPLOAD_VERSION  version label to publish under.
FROM alpine:3.20

RUN apk add --no-cache bash ca-certificates curl jq

COPY entrypoint.sh /usr/local/bin/gocdnext-artifactory
RUN chmod +x /usr/local/bin/gocdnext-artifactory

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-artifactory"]
