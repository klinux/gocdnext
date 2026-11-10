#!/usr/bin/env bash
# Mock-PATH unit test for the aws-cli plugin entrypoint. Stubs `aws` (records
# argv + the AWS_* env it inherits) so the dispatch is asserted without a real
# AWS account. Load-bearing cases:
#   - PLUGIN_COMMAND is required (exit 2) and is word-split into aws's argv;
#   - PLUGIN_PROFILE becomes `--profile <p>` AFTER the command;
#   - PLUGIN_REGION is exported as AWS_DEFAULT_REGION;
#   - the command word-split does NOT glob-expand against the workspace (#361):
#     a literal `*` must reach aws, never be rewritten into workspace filenames.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# Stub aws: log argv, and (for the region case) the inherited AWS_DEFAULT_REGION.
cat >"$TMP/aws" <<EOF
#!/usr/bin/env bash
printf 'aws %s\n' "\$*" >> "$TMP/aws.log"
printf 'REGION=%s\n' "\${AWS_DEFAULT_REGION:-<unset>}" >> "$TMP/env.log"
exit "\${MOCK_RC:-0}"
EOF
chmod +x "$TMP/aws"

fails=0
pass() { echo "PASS: $1"; }
bad()  { echo "FAIL: $1"; fails=1; }
# Run the entrypoint with the stub ahead on PATH; returns the entrypoint's code.
runep() { : >"$TMP/aws.log"; : >"$TMP/env.log"; env "$@" PATH="$TMP:$PATH" bash "$HERE/entrypoint.sh" >/dev/null 2>&1; }

echo "== 1. missing command -> exit 2 =="
runep PLUGIN_COMMAND=""; [ $? -eq 2 ] && pass "missing command exit 2" || bad "missing command did not exit 2"

echo "== 2. command word-split into aws argv =="
runep PLUGIN_COMMAND="s3 cp dist s3://bkt --recursive"
grep -qxF "aws s3 cp dist s3://bkt --recursive" "$TMP/aws.log" || bad "command not word-split verbatim: $(cat "$TMP/aws.log")"
[ "$fails" = 0 ] && pass "command word-split"

echo "== 3. profile -> --profile <p> after the command =="
runep PLUGIN_COMMAND="sts get-caller-identity" PLUGIN_PROFILE="ci"
grep -qxF "aws sts get-caller-identity --profile ci" "$TMP/aws.log" || bad "profile not appended: $(cat "$TMP/aws.log")"
grep -qxF "aws sts get-caller-identity --profile ci" "$TMP/aws.log" && pass "profile appended after command"

echo "== 4. region -> AWS_DEFAULT_REGION exported =="
runep PLUGIN_COMMAND="s3 ls" PLUGIN_REGION="eu-west-1"
grep -qxF "REGION=eu-west-1" "$TMP/env.log" || bad "region not exported: $(cat "$TMP/env.log")"
grep -qxF "REGION=eu-west-1" "$TMP/env.log" && pass "region exported as AWS_DEFAULT_REGION"

echo "== 5. word-split does NOT glob-expand against the workspace (#361) =="
mkdir -p "$TMP/ws"; : >"$TMP/ws/secret-a.txt"; : >"$TMP/ws/secret-b.txt"
runep PLUGIN_WORKING_DIR="$TMP/ws" PLUGIN_COMMAND="s3 cp * s3://bkt --recursive"
if grep -qxF "aws s3 cp * s3://bkt --recursive" "$TMP/aws.log"; then
  pass "glob not expanded (literal *)"
else
  bad "glob expanded against workspace — aws.log: $(cat "$TMP/aws.log")"
fi

[ "$fails" = 0 ] && echo "PASS: aws-cli entrypoint" || { echo "FAIL: aws-cli entrypoint"; exit 1; }
