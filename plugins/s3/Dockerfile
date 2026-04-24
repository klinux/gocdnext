# gocdnext/s3 — upload + download objects against an S3-compatible
# bucket. "S3-compatible" on purpose — the plugin uses the AWS CLI
# v2 so it drops into real AWS, MinIO, Backblaze B2, Cloudflare R2,
# DigitalOcean Spaces, and every other S3-compatible backend with
# a single endpoint-url swap. `action: upload | download` toggles
# direction, matching the shape of gocdnext/nexus + gocdnext/
# artifactory so pipelines read consistently across registries.
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_ACTION       (required)  "upload" | "download"
#   PLUGIN_BUCKET       (required)  bucket name (no "s3://" prefix).
#   PLUGIN_KEY          (required)  object key within the bucket.
#                                   For upload this is the
#                                   destination; for download it's
#                                   the source.
#   PLUGIN_REGION       (optional)  defaults to the caller's
#                                   AWS_DEFAULT_REGION env (which
#                                   the agent normally receives
#                                   via the run's env block).
#   PLUGIN_ENDPOINT_URL (optional)  for MinIO, R2, DO Spaces, etc.
#                                   e.g. https://s3.eu-central-1.wasabisys.com
#   PLUGIN_ACL          (optional)  upload: canned ACL
#                                   (private|public-read|...).
#                                   Default: leave untouched
#                                   (bucket policy wins).
#   PLUGIN_CONTENT_TYPE (optional)  upload: Content-Type metadata.
#                                   Default inferred from extension
#                                   by the AWS CLI.
#
#   # Upload-only:
#   PLUGIN_FILE         (required when action=upload)  workspace-
#                       relative source path.
#   # Download-only:
#   PLUGIN_DEST         (optional, download)  workspace-relative
#                       destination; defaults to filename from key.
#
# Credentials: the AWS CLI picks up AWS_ACCESS_KEY_ID /
# AWS_SECRET_ACCESS_KEY (+ optional AWS_SESSION_TOKEN) from the
# env — pipe them via the job's `secrets:` list + env map, same
# as gocdnext/aws-cli. IRSA / Workload Identity Federation both
# work automatically when the SDK finds their env markers.
FROM public.ecr.aws/aws-cli/aws-cli:2.22.8

# The upstream image ENTRYPOINT points at /usr/local/bin/aws.
# We replace it with our wrapper so a single container handles
# both directions with the same PLUGIN_* contract.
ENTRYPOINT []

RUN yum install -y bash && yum clean all

COPY entrypoint.sh /usr/local/bin/gocdnext-s3
RUN chmod +x /usr/local/bin/gocdnext-s3

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-s3"]
