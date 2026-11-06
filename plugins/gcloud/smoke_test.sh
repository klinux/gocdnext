#!/usr/bin/env bash
# Real smoke for the gcloud plugin — builds the PINNED IMAGE and runs the actual
# gcloud binary. No GCP account is reachable in CI, so it exercises everything
# that works offline: the binary + version, PLUGIN_COMMAND required, an offline
# subcommand through the word-split exec path, --project passthrough, and the
# service-account auth path (activate a throwaway key, confirm it's active, and
# confirm the key body never shows up in the output). Convention-named so
# plugins.yml runs it. Needs docker (+ openssl/jq on the host for the SA key).
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMG="gocdnext-plugin-gcloud:smoke-$$"
WORK="$(mktemp -d)"
cleanup() {
  docker image rm -f "$IMG" >/dev/null 2>&1 || true
  rm -rf "$WORK" || true
}
trap cleanup EXIT

die() { echo "FAIL: $1"; [ -n "${2:-}" ] && { echo "--- output ---"; echo "$2"; }; exit 1; }

echo "==> build (same Dockerfile/context as the official build)"
docker build -t "$IMG" "$HERE" >/dev/null

# plug <extra docker/env args...> -> OUT, RC  (never trips set -e)
plug() {
  set +e
  OUT="$(docker run --rm -e HOME=/root "$@" "$IMG" 2>&1)"; RC=$?
  set -e
}

# 1. binary present + version runs through the plugin.
plug -e PLUGIN_COMMAND=version
[ "$RC" -eq 0 ] || die "gcloud version should run (rc=$RC)" "$OUT"
grep -qiE "Google Cloud SDK [0-9]+" <<<"$OUT" || die "no version banner" "$OUT"

# 2. PLUGIN_COMMAND required.
plug
[ "$RC" -eq 2 ] || die "missing PLUGIN_COMMAND should exit 2 (rc=$RC)" "$OUT"

# 3. the subcommands the plugin documents still exist and expose their flags in
#    592 (offline `--help` through the word-split exec path). A removed command or
#    renamed flag — the real risk of the 502→592 major jump — fails here.
#    PAGER=cat so gcloud's full --help doesn't invoke a pager (non-zero without a TTY).
help_has() { # <command words> <expected flag>
  plug -e PAGER=cat -e PLUGIN_COMMAND="$1 --help"
  [ "$RC" -eq 0 ] || die "'$1 --help' should run offline (rc=$RC)" "$OUT"
  grep -q -- "$2" <<<"$OUT" || die "'$1 --help' no longer documents '$2'" "$OUT"
}
help_has "run deploy" "--image"
help_has "builds submit" "--tag"
help_has "container clusters get-credentials" "--region"
help_has "storage cp" "--recursive"

# 4. --project passthrough shows on the exec banner.
plug -e PAGER=cat -e PLUGIN_COMMAND="run deploy --help" -e PLUGIN_PROJECT=demo-project
[ "$RC" -eq 0 ] || die "command with --project should run (rc=$RC)" "$OUT"
grep -q -- "--project demo-project" <<<"$OUT" || die "project not passed through on the banner" "$OUT"

# 5. service-account auth path on the real binary. A throwaway key can't truly
#    activate — gcloud refreshes the token against Google — so this asserts the
#    plugin's offline-observable contract: the activation step runs, it fails
#    CLOSED (non-zero, not a spurious success), and the key body never appears
#    in the output (it goes to a 0600 file + --key-file, never argv/logs). The
#    deterministic argv/no-leak contract is covered in entrypoint_test.sh.
KEY="$(openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 2>/dev/null)"
SA_JSON="$(jq -nc --arg k "$KEY" '{
  type:"service_account", project_id:"demo", private_key_id:"deadbeef",
  private_key:$k, client_email:"smoke@demo.iam.gserviceaccount.com",
  client_id:"1", token_uri:"https://oauth2.googleapis.com/token"
}')"
plug -e PLUGIN_COMMAND="auth list" -e PLUGIN_CREDENTIALS_JSON="$SA_JSON"
[ "$RC" -ne 0 ] || die "a throwaway SA key must not spuriously activate — expected fail-closed" "$OUT"
grep -q "activating service account from secrets" <<<"$OUT" \
  || die "activation step did not run" "$OUT"
if grep -qE "BEGIN (RSA )?PRIVATE KEY|private_key" <<<"$OUT"; then
  die "SA key body leaked into output" "$OUT"
fi

echo "PASS: gcloud smoke"
