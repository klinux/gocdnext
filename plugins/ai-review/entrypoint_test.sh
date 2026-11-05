#!/usr/bin/env bash
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"

for bad in 0.5.6 .5 0. 01 1.1; do
  out="$(env PLUGIN_PROVIDER=openai OPENAI_API_KEY=dummy PLUGIN_TEMPERATURE="$bad" \
    bash "$HERE/entrypoint.sh" 2>&1)"
  rc=$?
  if [ "$rc" -ne 2 ] || ! printf '%s\n' "$out" | grep -q 'decimal in \[0, 1\]'; then
    echo "FAIL: malformed temperature '$bad' should fail validation with rc=2"
    echo "$out"
    exit 1
  fi
done

echo "PASS: ai-review entrypoint"
