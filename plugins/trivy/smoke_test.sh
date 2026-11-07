#!/usr/bin/env bash
# Real smoke for the trivy plugin's IMAGE. Proves the CVE bump (0.56.2 -> 0.76.2)
# left the scanner working end to end, across both the offline and online paths,
# and that the misconfig path stays skew-free (the reason the entrypoint uses
# --skip-check-update — a newer registry check bundle against an old binary used
# to emit `requestedamis` / "Failed to find embedded check" noise):
#   - the binary is v0.76.2,
#   - a `config` scan (embedded checks, offline) FLAGS a privileged pod and exits
#     non-zero, with NO embedded-check/rego skew error, and PASSES a clean dir,
#   - an `fs` scan DETECTS a known CVE (lodash 4.17.4 / CVE-2019-10744) and exits
#     non-zero, and PASSES a clean dir.
#
# Convention-named so plugins.yml runs it as a PRE-PUBLISH gate. Needs docker +
# network (the fs scan pulls Trivy's vuln DB). Built --no-cache to match the
# plugin's no_cache publish build. Trap cleanup.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMG="gocdnext-plugin-trivy:smoke-$$"
W="$(mktemp -d)"
cleanup() { docker image rm -f "$IMG" >/dev/null 2>&1 || true; rm -rf "$W" 2>/dev/null || true; }
trap cleanup EXIT
die() { echo "FAIL: $1"; [ -n "${2:-}" ] && { echo "--- output ---"; echo "$2" | tail -40; }; exit 1; }

echo "==> build plugin image (--no-cache, same Dockerfile/context as the official build)"
docker build --no-cache -t "$IMG" "$HERE" >/dev/null

echo "==> trivy binary is v0.76.2"
OUT="$(docker run --rm --entrypoint trivy "$IMG" --version 2>&1)" || die "trivy --version failed" "$OUT"
grep -q "0.76.2" <<<"$OUT" || die "trivy is not v0.76.2" "$OUT"

# Fixtures: a privileged pod (misconfig), a vulnerable lockfile (CVE), a clean dir.
mkdir -p "$W/cfg" "$W/vuln" "$W/clean"
cat >"$W/cfg/pod.yaml" <<'YAML'
apiVersion: v1
kind: Pod
metadata: { name: smoke }
spec:
  containers:
    - name: c
      image: nginx
      securityContext: { privileged: true }
YAML
cat >"$W/vuln/package-lock.json" <<'JSON'
{"name":"x","version":"1.0.0","lockfileVersion":3,"requires":true,
 "packages":{"node_modules/lodash":{"version":"4.17.4","resolved":"https://registry.npmjs.org/lodash/-/lodash-4.17.4.tgz","integrity":"sha1-eCA6TRwyiuHYbcpkYONptX9AVa4="}}}
JSON
printf 'nothing to see here\n' >"$W/clean/readme.txt"

run() { # run <subdir> <extra -e ...> : sets RC + OUT
  local sub="$1"; shift
  set +e
  OUT="$(docker run --rm -v "$W/$sub:/workspace" -w /workspace "$@" "$IMG" 2>&1)"; RC=$?
  set -e
}

echo "==> config scan flags a privileged pod (offline, embedded checks, no skew)"
run cfg -e PLUGIN_SCAN_TYPE=config -e PLUGIN_EXIT_CODE=1
[ "$RC" -ne 0 ] || die "config scan should fail on a privileged pod (rc=$RC)" "$OUT"
grep -q "KSV-0017" <<<"$OUT" || die "config scan did not flag the privileged container (KSV-0017)" "$OUT"
if grep -qiE 'Failed to find embedded check|requestedamis|undefined ref' <<<"$OUT"; then
  die "config scan hit a check-skew/rego error" "$OUT"
fi

echo "==> config scan passes a clean dir"
run clean -e PLUGIN_SCAN_TYPE=config -e PLUGIN_EXIT_CODE=1
[ "$RC" -eq 0 ] || die "config scan of a clean dir should pass (rc=$RC)" "$OUT"

echo "==> fs scan detects a known CVE (lodash 4.17.4)"
run vuln -e PLUGIN_SCAN_TYPE=fs -e PLUGIN_EXIT_CODE=1 -e PLUGIN_SEVERITY=HIGH,CRITICAL -e PLUGIN_IGNORE_UNFIXED=false
[ "$RC" -ne 0 ] || die "fs scan should fail on vulnerable lodash (rc=$RC)" "$OUT"
grep -q "CVE-2019-10744" <<<"$OUT" || die "fs scan did not report the known lodash CVE" "$OUT"

echo "==> fs scan passes a clean dir"
run clean -e PLUGIN_SCAN_TYPE=fs -e PLUGIN_EXIT_CODE=1
[ "$RC" -eq 0 ] || die "fs scan of a clean dir should pass (rc=$RC)" "$OUT"

echo "PASS: trivy smoke"
