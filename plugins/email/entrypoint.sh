#!/bin/bash
# gocdnext/email — validate required inputs then delegate to
# send.py for the actual SMTP. Keeping the bash wrapper thin so
# Python owns all the network / error handling.

set -euo pipefail

for var in PLUGIN_HOST PLUGIN_FROM PLUGIN_TO PLUGIN_SUBJECT PLUGIN_BODY; do
    if [ -z "${!var:-}" ]; then
        echo "gocdnext/email: ${var,,} is required" >&2
        exit 2
    fi
done

exec python3 /usr/local/bin/gocdnext-email-send.py
