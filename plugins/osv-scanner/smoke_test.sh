#!/usr/bin/env bash
# Real smoke for the osv-scanner plugin's IMAGE. Runs the PINNED binary end to
# end so a bump that renames a flag (scan source / --format / --output) or breaks
# the exit mapping fails HERE, before publish. Proves the bump (osv-scanner
# v2.0.0 -> v2.7.0, go1.27, alpine 3.24.4):
#   - the binary is v2.7.0;
#   - a workspace with NO lockfiles is a clean pass + a parser-valid empty SARIF
#     (offline — no OSV.dev query);
#   - a lockfile pinning a known-vulnerable package is DETECTED: fail-on=any ->
#     exit 1 with findings in the SARIF, and fail-on=none -> exit 0 (report-only)
#     with the findings still recorded.
#
# The detection cases query api.osv.dev (that is the scanner's whole job); the CI
# runner has network. Convention-named so plugins.yml runs it as a PRE-PUBLISH
# gate. Needs docker. Built --no-cache to match the no_cache publish build.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMG="gocdnext-plugin-osv-scanner:smoke-$$"
W="$(mktemp -d)"       # vuln workspace
WE="$(mktemp -d)"      # empty workspace (no sources)
SMOKE_OK=0
cleanup() {
  # osv-scanner writes the report as root into the mounts; remove it from inside
  # the image (matching uid) before deleting the dirs, then verify they're gone.
  if docker image inspect "$IMG" >/dev/null 2>&1; then
    docker run --rm --entrypoint sh -v "$W:/a" -v "$WE:/b" "$IMG" \
      -c 'rm -rf /a/* /a/.[!.]* /b/* /b/.[!.]* 2>/dev/null || true' >/dev/null 2>&1 || true
  fi
  docker image rm -f "$IMG" >/dev/null 2>&1 || true
  rm -rf "$W" "$WE" 2>/dev/null || true
  if [ -d "$W" ] || [ -d "$WE" ]; then
    echo "WARN: cleanup could not fully remove the workspaces" >&2
    [ "$SMOKE_OK" = 1 ] && { echo "FAIL: smoke passed but left files behind" >&2; exit 1; }
  fi
}
trap cleanup EXIT
die() { echo "FAIL: $1"; [ -n "${2:-}" ] && { echo "--- output ---"; echo "$2" | tail -40; }; exit 1; }
count() { docker run --rm -v "$1:/w" --entrypoint jq "$IMG" '[.runs[].results[]] | length' "/w/$2" 2>/dev/null; }
no_deprecation() { grep -qi 'deprecat' <<<"$2" && die "$1: wrapper used a deprecated osv-scanner flag" "$2" || true; }

echo "==> build plugin image (--no-cache, same Dockerfile/context as the official build)"
docker build --no-cache -t "$IMG" "$HERE" >/dev/null

echo "==> osv-scanner is v2.7.0"
OUT="$(docker run --rm --entrypoint osv-scanner "$IMG" --version 2>&1)" || die "osv-scanner --version failed" "$OUT"
grep -q '2.7.0' <<<"$OUT" || die "osv-scanner is not v2.7.0" "$OUT"

echo "==> no lockfiles -> clean pass + empty SARIF (offline, --network none)"
set +e
OUT="$(docker run --rm --network none -v "$WE:/workspace" -w /workspace "$IMG" 2>&1)"; RC=$?
set -e
[ "$RC" -eq 0 ] || die "no-sources: expected exit 0, got $RC" "$OUT"
grep -qi 'nothing to scan' <<<"$OUT" || die "no-sources: expected the clean 'nothing to scan' path" "$OUT"
[ "$(count "$WE" osv-report.sarif)" = "0" ] || die "no-sources: empty SARIF not synthesized" "$OUT"
no_deprecation "no-sources" "$OUT"

echo "==> a known-vulnerable lockfile is detected"
cat >"$W/package-lock.json" <<'JSON'
{
  "name": "osv-smoke", "version": "1.0.0", "lockfileVersion": 3, "requires": true,
  "packages": {
    "": { "name": "osv-smoke", "version": "1.0.0", "dependencies": { "lodash": "4.17.4" } },
    "node_modules/lodash": {
      "version": "4.17.4",
      "resolved": "https://registry.npmjs.org/lodash/-/lodash-4.17.4.tgz",
      "integrity": "sha1-eCA6TRwyiuHYbcpkYONptX9AVa4="
    }
  }
}
JSON

echo "   fail-on=any -> exit 1 with findings"
set +e
OUT="$(docker run --rm -v "$W:/workspace" -w /workspace -e PLUGIN_FAIL_ON=any "$IMG" 2>&1)"; RC=$?
set -e
[ "$RC" -eq 1 ] || die "vuln/any: expected exit 1, got $RC" "$OUT"
n="$(count "$W" osv-report.sarif)"
[ -n "$n" ] && [ "$n" -gt 0 ] 2>/dev/null || die "vuln/any: SARIF has no findings (got '${n}')" "$OUT"
no_deprecation "vuln/any" "$OUT"
echo "   (found ${n} results)"

echo "   fail-on=none -> exit 0 (report-only), findings still recorded"
set +e
OUT="$(docker run --rm -v "$W:/workspace" -w /workspace -e PLUGIN_FAIL_ON=none "$IMG" 2>&1)"; RC=$?
set -e
[ "$RC" -eq 0 ] || die "vuln/none: expected exit 0 (report-only), got $RC" "$OUT"
n="$(count "$W" osv-report.sarif)"
[ -n "$n" ] && [ "$n" -gt 0 ] 2>/dev/null || die "vuln/none: SARIF lost its findings (got '${n}')" "$OUT"
no_deprecation "vuln/none" "$OUT"

SMOKE_OK=1
echo "PASS: osv-scanner smoke"
