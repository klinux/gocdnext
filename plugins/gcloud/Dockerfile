# gocdnext/gcloud — run Google Cloud SDK (gcloud) commands.
#
# Auth shape (choose one, ordered by "how 2026 teams actually do
# it"):
#   1. Service account JSON key — PLUGIN_CREDENTIALS_JSON from
#      secrets. Written to a 0600 tempfile and activated. Legacy
#      but still the most common in CI.
#   2. Workload Identity Federation — set GOOGLE_APPLICATION_CREDENTIALS
#      to a credential config file in the workspace and leave
#      PLUGIN_CREDENTIALS_JSON empty. No static keys.
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_COMMAND          (required)  gcloud subcommand + args,
#                                       word-split. Example:
#                                         "run deploy my-svc --image gcr.io/p/app:1 --region us-east1"
#   PLUGIN_PROJECT          (optional)  --project ID shortcut so
#                                       each command doesn't have
#                                       to pass it.
#   PLUGIN_CREDENTIALS_JSON (optional)  raw JSON body of a SA key.
#                                       Populate via secrets:.
#   PLUGIN_WORKING_DIR      (optional)  cd here first. Default ".".
FROM google/cloud-sdk:502.0.0-alpine

RUN apk add --no-cache bash

COPY entrypoint.sh /usr/local/bin/gocdnext-gcloud
RUN chmod +x /usr/local/bin/gocdnext-gcloud

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-gcloud"]
