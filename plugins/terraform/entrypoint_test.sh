#!/usr/bin/env bash
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/bin" "$TMP/work/infra"

cat >"$TMP/bin/terraform-bundled" <<'STUB'
#!/usr/bin/env bash
printf 'bundled|%s|%s\n' "$PWD" "$*" >"$CALL_FILE"
STUB
cat >"$TMP/bin/tfenv" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >"$TFENV_CALL_FILE"
dest="${TFENV_CONFIG_DIR}/versions/${TFENV_TERRAFORM_VERSION}/terraform"
mkdir -p "$(dirname "$dest")"
cat >"$dest" <<'TERRAFORM'
#!/usr/bin/env bash
printf 'dynamic|%s|%s\n' "$PWD" "$*" >"$CALL_FILE"
TERRAFORM
chmod +x "$dest"
STUB
printf '#!/usr/bin/env bash\nexit 0\n' >"$TMP/bin/git"
chmod +x "$TMP/bin/terraform-bundled" "$TMP/bin/tfenv" "$TMP/bin/git"

OUT=""; RC=0
run() {
    : >"$TMP/call"; : >"$TMP/tfenv-call"
    OUT="$(cd "$TMP/work" && env -i \
        PATH="$TMP/bin:/usr/bin:/bin" \
        CALL_FILE="$TMP/call" TFENV_CALL_FILE="$TMP/tfenv-call" \
        BUNDLED_TERRAFORM_BIN="$TMP/bin/terraform-bundled" \
        TFENV_BIN="$TMP/bin/tfenv" TFENV_CONFIG_DIR="$TMP/tfenv-cache" \
        "$@" bash "$HERE/entrypoint.sh" 2>&1)"
    RC=$?
}
fail() { echo "FAIL: $1"; echo "--- output ---"; echo "$OUT"; echo "--- terraform ---"; cat "$TMP/call"; echo "--- tfenv ---"; cat "$TMP/tfenv-call"; exit 1; }

run PLUGIN_COMMAND=version
[ "$RC" -eq 0 ] || fail "default version should run"
grep -qx "bundled|$TMP/work|version" "$TMP/call" || fail "latest must use bundled Terraform"
[ ! -s "$TMP/tfenv-call" ] || fail "latest must not invoke tfenv"

run PLUGIN_COMMAND=version PLUGIN_VERSION=1.17.1
[ "$RC" -eq 0 ] || fail "bundled exact version should run"
[ ! -s "$TMP/tfenv-call" ] || fail "bundled exact version must not invoke tfenv"

run PLUGIN_COMMAND='plan *.tf' PLUGIN_VERSION=1.8.5 PLUGIN_WORKING_DIR=infra PLUGIN_VAR_FILE=prod.tfvars
[ "$RC" -eq 0 ] || fail "exact version should install and run"
grep -qx 'install 1.8.5' "$TMP/tfenv-call" || fail "tfenv install mismatch"
grep -qx "dynamic|$TMP/work/infra|plan \*.tf -var-file prod.tfvars" "$TMP/call" || fail "dynamic command/args mismatch or glob expanded"

run PLUGIN_COMMAND=version PLUGIN_VERSION=latest-allowed
[ "$RC" -eq 2 ] || fail "moving/non-exact selector must fail"
[ ! -s "$TMP/tfenv-call" ] || fail "invalid version must fail before tfenv"

run
[ "$RC" -eq 2 ] || fail "missing command must fail"

echo "PASS: terraform entrypoint"
