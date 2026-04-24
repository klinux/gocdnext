#!/bin/bash
# gocdnext/s3 — wrapper around `aws s3 cp` for upload/download.
# See Dockerfile for the full contract.

set -euo pipefail

for var in PLUGIN_ACTION PLUGIN_BUCKET PLUGIN_KEY; do
    if [ -z "${!var:-}" ]; then
        echo "gocdnext/s3: ${var,,} is required" >&2
        exit 2
    fi
done

bucket="${PLUGIN_BUCKET}"
key="${PLUGIN_KEY}"
s3_uri="s3://${bucket}/${key#/}"

aws_args=()
if [ -n "${PLUGIN_REGION:-}" ]; then
    aws_args+=(--region "${PLUGIN_REGION}")
fi
if [ -n "${PLUGIN_ENDPOINT_URL:-}" ]; then
    # --endpoint-url is how the AWS CLI speaks to MinIO / R2 /
    # DO Spaces / any S3-compatible backend. Kept at the
    # invocation level, not baked into the container env, so a
    # pipeline can multiplex between AWS and a self-hosted
    # bucket without rebuilding the plugin.
    aws_args+=(--endpoint-url "${PLUGIN_ENDPOINT_URL}")
fi

case "${PLUGIN_ACTION}" in
    upload)
        file_rel="${PLUGIN_FILE:-}"
        if [ -z "${file_rel}" ]; then
            echo "gocdnext/s3: file is required for action=upload" >&2
            exit 2
        fi
        src="/workspace/${file_rel#/}"
        if [ ! -f "${src}" ]; then
            echo "gocdnext/s3: ${file_rel} not found in workspace" >&2
            exit 2
        fi
        extra=()
        if [ -n "${PLUGIN_ACL:-}" ]; then
            extra+=(--acl "${PLUGIN_ACL}")
        fi
        if [ -n "${PLUGIN_CONTENT_TYPE:-}" ]; then
            extra+=(--content-type "${PLUGIN_CONTENT_TYPE}")
        fi
        echo "==> aws s3 cp ${src} ${s3_uri}"
        exec aws "${aws_args[@]}" s3 cp "${src}" "${s3_uri}" "${extra[@]}"
        ;;

    download)
        dest_rel="${PLUGIN_DEST:-}"
        if [ -z "${dest_rel}" ]; then
            # Default: drop the object next to the script's cwd
            # with the key's basename.
            dest_rel="$(basename "${key}")"
        fi
        dest="/workspace/${dest_rel#/}"
        # If dest is a directory, write the object to <dir>/<basename>.
        # Mirrors curl -o semantics the other registry plugins
        # already use.
        if [ -d "${dest}" ]; then
            dest="${dest%/}/$(basename "${key}")"
        else
            mkdir -p "$(dirname "${dest}")"
        fi
        echo "==> aws s3 cp ${s3_uri} ${dest}"
        exec aws "${aws_args[@]}" s3 cp "${s3_uri}" "${dest}"
        ;;

    *)
        echo "gocdnext/s3: unknown action '${PLUGIN_ACTION}' (accepted: upload, download)" >&2
        exit 2
        ;;
esac
