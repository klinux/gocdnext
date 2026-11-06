#!/usr/bin/env bash
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/bin"
printf '#!/usr/bin/env sh\nprintf "{}"\n' >"$TMP/bin/jq"
cat >"$TMP/bin/curl" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >"$ARGS_FILE"
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--config" ]; then shift; cp "$1" "$CONFIG_FILE"; fi
  shift
done
STUB
chmod +x "$TMP/bin/jq" "$TMP/bin/curl"

webhook='https://discord.example/api/webhooks/id/super-secret'
out="$(env PATH="$TMP/bin:/usr/bin:/bin" ARGS_FILE="$TMP/args" CONFIG_FILE="$TMP/config" \
  PLUGIN_WEBHOOK="$webhook" PLUGIN_CONTENT='hello' bash "$HERE/entrypoint.sh" 2>&1)"
rc=$?
[ "$rc" -eq 0 ] || { echo "FAIL: discord entrypoint rc=$rc"; echo "$out"; exit 1; }
case "$out" in *super-secret*|*discord.example*) echo "FAIL: webhook leaked to log"; exit 1 ;; esac
case "$(cat "$TMP/args")" in *super-secret*) echo "FAIL: webhook leaked to argv"; exit 1 ;; esac
grep -Fqx "url = \"$webhook\"" "$TMP/config" || { echo "FAIL: curl config lost webhook"; exit 1; }

echo "PASS: discord entrypoint"
