#!/usr/bin/env bash
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/bin" "$TMP/work/nested"
printf '#!/usr/bin/env sh\nprintf "{}"\n' >"$TMP/bin/jq"
cat >"$TMP/bin/curl" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$PWD" >"$PWD_FILE"
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--output" ]; then shift; : >"$1"; fi
  shift
done
printf '204'
STUB
chmod +x "$TMP/bin/jq" "$TMP/bin/curl"

out="$(cd "$TMP/work" && env PATH="$TMP/bin:/usr/bin:/bin" PWD_FILE="$TMP/pwd" \
  PLUGIN_WORKING_DIR=nested PLUGIN_PROVIDER=datadog DATADOG_API_KEY=secret \
  sh "$HERE/entrypoint.sh" 2>&1)"
rc=$?
[ "$rc" -eq 0 ] || { echo "FAIL: deploy-marker rc=$rc"; echo "$out"; exit 1; }
grep -Fqx "$TMP/work/nested" "$TMP/pwd" || { echo "FAIL: working-dir was ignored"; exit 1; }

echo "PASS: deploy-marker entrypoint"
