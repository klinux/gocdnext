#!/usr/bin/env bash
# Real smoke for the semgrep plugin's IMAGE. Runs the PINNED wrapper end to end
# against throwaway workspaces so the big base bump (semgrep 1.96.0 -> 1.184.0,
# alpine 3.20 -> 3.23.8, Python 3.11 -> 3.12) that could move the CLI surface,
# SARIF shape, exit codes, or the severity gate fails HERE, before publish.
# Fully OFFLINE: every scan uses a LOCAL rule file (registry packs like p/default
# need network) and runs under --network none. Proves:
#   - semgrep is 1.184.0 and certifi is >= 2026.10.18 (the removed CVE source);
#   - fail-on is validated (bad value -> exit 2);
#   - a matching rule produces a finding in the SARIF, and the gate maps it:
#       fail-on=error -> exit 1 (an error-severity finding gates),
#       fail-on=none  -> exit 0 but the SARIF STILL carries the finding
#                        (the artifact never loses findings to the gate);
#   - a clean tree -> exit 0 with an empty-results SARIF;
#   - a WARNING-severity finding does NOT gate fail-on=error but DOES gate
#     fail-on=warning (proves the jq ruleId->level resolution, since semgrep
#     does not stamp .level on each SARIF result);
#   - a broken --config (scan error) propagates loud (exit >= 2), not masked;
#   - --baseline-commit reports only findings NEW since a commit.
# NB: this proves the wrapper CONTRACT, not that findings match 1.96.0 — the
# engine and bundled rules evolved across 88 minors, so results may legitimately
# move. The rules here are local and fixed, so the contract assertions are stable.
#
# Convention-named so plugins.yml runs it as a PRE-PUBLISH gate. Needs docker
# (+ git on the host for the baseline fixture). Built --no-cache to match the
# no_cache publish build. Trap cleanup, fail-closed on leftovers.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMG="gocdnext-plugin-semgrep:smoke-$$"
WE="$(mktemp -d)"   # error-severity finding
WC="$(mktemp -d)"   # clean tree
WW="$(mktemp -d)"   # warning-severity finding
WB="$(mktemp -d)"   # baseline: git repo, new finding after a commit
SMOKE_OK=0
cleanup() {
  # semgrep writes the SARIF into the mount as root (the image's default user);
  # remove it from inside the image before deleting the dirs, then verify.
  if docker image inspect "$IMG" >/dev/null 2>&1; then
    docker run --rm --entrypoint sh -v "$WE:/e" -v "$WC:/c" -v "$WW:/w" -v "$WB:/b" "$IMG" \
      -c 'for d in /e /c /w /b; do rm -rf "$d"/* "$d"/.[!.]* "$d"/..?* 2>/dev/null || true; done' \
      >/dev/null 2>&1 || true
  fi
  docker image rm -f "$IMG" >/dev/null 2>&1 || true
  rm -rf "$WE" "$WC" "$WW" "$WB" 2>/dev/null || true
  leftover=0
  for d in "$WE" "$WC" "$WW" "$WB"; do
    [ -d "$d" ] && { echo "WARN: cleanup could not fully remove ${d}" >&2; leftover=1; }
  done
  if docker image inspect "$IMG" >/dev/null 2>&1; then
    echo "WARN: smoke image ${IMG} was not removed" >&2; leftover=1
  fi
  [ "$leftover" = 1 ] && [ "$SMOKE_OK" = 1 ] && { echo "FAIL: smoke passed but left state behind" >&2; exit 1; }
  return 0
}
trap cleanup EXIT
die() { echo "FAIL: $1"; [ -n "${2:-}" ] && { echo "--- output ---"; echo "$2" | tail -40; }; exit 1; }

# A local rule file (offline). $1=dir, $2=severity (ERROR|WARNING).
rule() {
  mkdir -p "$1/rules"
  cat >"$1/rules/no-eval.yaml" <<YAML
rules:
  - id: no-eval
    languages: [python]
    message: avoid eval
    severity: $2
    pattern: eval(...)
YAML
}
bad()   { printf 'def f(x):\n    return eval(x)\n' >"$1/bad.py"; }
clean() { printf 'def f(x):\n    return int(x)\n'  >"$1/ok.py"; }
# run the wrapper (its real entrypoint) in $1, offline. Sets RC + OUT.
run() { d="$1"; shift; set +e; OUT="$(docker run --rm --network none -v "$d:/workspace" -w /workspace "$@" "$IMG" 2>&1)"; RC=$?; set -e; }
# count SARIF results inside the image (jq is baked in).
sarif_count() { docker run --rm --network none -v "$1:/w" -w /w --entrypoint jq "$IMG" '[.runs[].results[]] | length' "$2" 2>/dev/null; }

echo "==> build plugin image (--no-cache, same Dockerfile/context as the official build)"
docker build --no-cache -t "$IMG" "$HERE" >/dev/null

echo "==> semgrep is 1.184.0 and certifi is patched (>= 2026.10.18, the removed CVE source)"
OUT="$(docker run --rm --network none --entrypoint semgrep "$IMG" --version 2>&1)" || die "semgrep --version failed" "$OUT"
grep -qF '1.184.0' <<<"$OUT" || die "semgrep is not 1.184.0 (got: ${OUT})" "$OUT"
CERTIFI="$(docker run --rm --network none --entrypoint python3 "$IMG" -c 'import certifi; print(certifi.__version__)' 2>&1)" \
  || die "could not read certifi version" "$CERTIFI"
# certifi versions are date-stamped YYYY.MM.DD; string compare is correct ordering.
[ "$(printf '%s\n2026.10.18\n' "$CERTIFI" | sort | head -1)" = "2026.10.18" ] \
  || die "certifi ${CERTIFI} is older than the 2026.10.18 fix (GHSA-6q48-6c7w-9gw2)"

echo "==> fail-on is validated: a bad value exits 2"
run "$WC" -e PLUGIN_FAIL_ON="nonsense" -e PLUGIN_CONFIG="rules/no-eval.yaml"
[ "$RC" -eq 2 ] || die "bad-fail-on: expected exit 2, got $RC" "$OUT"

echo "==> ERROR finding: fail-on=error gates (exit 1) and the SARIF carries the finding"
rule "$WE" ERROR; bad "$WE"
run "$WE" -e PLUGIN_CONFIG="rules/no-eval.yaml" -e PLUGIN_FAIL_ON="error"
[ "$RC" -eq 1 ] || die "error-gate: expected exit 1, got $RC" "$OUT"
[ "$(sarif_count "$WE" semgrep.sarif)" = "1" ] || die "error-gate: SARIF should carry exactly 1 finding" "$OUT"

echo "==> same finding, fail-on=none: exit 0 but the SARIF STILL carries it (gate never drops findings)"
run "$WE" -e PLUGIN_CONFIG="rules/no-eval.yaml" -e PLUGIN_FAIL_ON="none"
[ "$RC" -eq 0 ] || die "none-gate: expected exit 0, got $RC" "$OUT"
[ "$(sarif_count "$WE" semgrep.sarif)" = "1" ] || die "none-gate: the SARIF must still carry the finding" "$OUT"

echo "==> clean tree: exit 0 with an empty-results SARIF"
rule "$WC" ERROR; clean "$WC"
run "$WC" -e PLUGIN_CONFIG="rules/no-eval.yaml" -e PLUGIN_FAIL_ON="error"
[ "$RC" -eq 0 ] || die "clean: expected exit 0, got $RC" "$OUT"
[ "$(sarif_count "$WC" semgrep.sarif)" = "0" ] || die "clean: SARIF should have 0 findings" "$OUT"

echo "==> WARNING finding: does NOT gate fail-on=error, DOES gate fail-on=warning"
# semgrep does not stamp .level on each SARIF result; the wrapper's jq resolves
# severity via ruleId -> rule.defaultConfiguration.level. This proves that path.
rule "$WW" WARNING; bad "$WW"
run "$WW" -e PLUGIN_CONFIG="rules/no-eval.yaml" -e PLUGIN_FAIL_ON="error"
[ "$RC" -eq 0 ] || die "warn-vs-error: a WARNING must not gate fail-on=error (got $RC)" "$OUT"
[ "$(sarif_count "$WW" semgrep.sarif)" = "1" ] || die "warn-vs-error: SARIF should still carry the warning" "$OUT"
grep -q 'warning=1' <<<"$OUT" || die "warn-vs-error: the gate did not resolve the finding as a warning" "$OUT"
run "$WW" -e PLUGIN_CONFIG="rules/no-eval.yaml" -e PLUGIN_FAIL_ON="warning"
[ "$RC" -eq 1 ] || die "warn-gate: a WARNING must gate fail-on=warning (got $RC)" "$OUT"

echo "==> broken --config (scan error) propagates loud (exit >= 2), not masked as a clean pass"
run "$WC" -e PLUGIN_CONFIG="rules/does-not-exist.yaml" -e PLUGIN_FAIL_ON="error"
[ "$RC" -ge 2 ] || die "scan-error: a broken config must propagate (expected >= 2), got $RC" "$OUT"

echo "==> baseline-commit reports only findings NEW since the commit"
rule "$WB" ERROR; clean "$WB"
git -C "$WB" init -q -b main
git -C "$WB" -c user.email=smoke@example.invalid -c user.name=smoke add -A
git -C "$WB" -c user.email=smoke@example.invalid -c user.name=smoke commit -qm baseline
BASE="$(git -C "$WB" rev-parse HEAD)"
bad "$WB"   # the NEW finding, in a tracked change
git -C "$WB" -c user.email=smoke@example.invalid -c user.name=smoke add -A
git -C "$WB" -c user.email=smoke@example.invalid -c user.name=smoke commit -qm introduce-eval
run "$WB" -e PLUGIN_CONFIG="rules/no-eval.yaml" -e PLUGIN_FAIL_ON="none" -e PLUGIN_BASELINE_COMMIT="$BASE"
[ "$RC" -eq 0 ] || die "baseline: expected exit 0 (fail-on=none), got $RC" "$OUT"
[ "$(sarif_count "$WB" semgrep.sarif)" = "1" ] || die "baseline: should report exactly the 1 NEW finding" "$OUT"

SMOKE_OK=1
echo "PASS: semgrep smoke"
