#!/usr/bin/env bash
# Real smoke for the netlify plugin — builds the PINNED IMAGE and runs the actual
# netlify-cli. A deploy can't complete without a real account, so it validates
# everything observable without one, with a focus on the deploy-only hardening:
#   - the bundled local-functions-proxy executable was removed (and stays gone);
#   - netlify-cli is the pinned 27.16.2;
#   - `netlify deploy --help` still documents --dir / --no-build / --prod / --alias
#     (the flags this plugin emits — the 17→27 major jump mustn't drop them);
#   - a real dir + throwaway creds runs the deploy codepath to an EXPECTED
#     auth/API failure, with NO MODULE_NOT_FOUND / ENOENT / local-functions-proxy
#     — proving the removal didn't break the deploy path.
# Convention-named so plugins.yml runs it. Needs docker + network (reaches the
# Netlify API to fail auth).
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMG="gocdnext-plugin-netlify:smoke-$$"
WORK="$(mktemp -d)"
cleanup() { docker image rm -f "$IMG" >/dev/null 2>&1 || true; rm -rf "$WORK" || true; }
trap cleanup EXIT

die() { echo "FAIL: $1"; [ -n "${2:-}" ] && { echo "--- output ---"; echo "$2"; }; exit 1; }

echo "==> build (same Dockerfile/context as the official build)"
docker build -t "$IMG" "$HERE" >/dev/null

# 1. the dev-only local-functions-proxy executable is gone (hardening held).
LEFT="$(docker run --rm --entrypoint sh "$IMG" -c \
  'find /usr/local/lib/node_modules/netlify-cli/node_modules -type f -path "*/@netlify/local-functions-proxy-*/bin/local-functions-proxy" | wc -l')"
[ "$LEFT" = "0" ] || die "local-functions-proxy executable still present ($LEFT) — hardening did not hold"

# 2. pinned CLI version.
VER="$(docker run --rm --entrypoint netlify "$IMG" --version 2>&1 || true)"
grep -q "netlify-cli/27.16.2" <<<"$VER" || die "unexpected netlify-cli version" "$VER"

# 3. deploy --help still documents the flags this plugin emits.
HELP="$(docker run --rm --entrypoint netlify "$IMG" deploy --help 2>&1 || true)"
for f in -- --dir --no-build --prod --alias; do
  grep -q -- "$f" <<<"$HELP" || die "netlify deploy --help no longer documents $f" "$HELP"
done

# plug <extra env...> -> OUT, RC against a workspace holding ./site
mkdir -p "$WORK/site"; printf '<!doctype html><title>smoke</title>\n' > "$WORK/site/index.html"
plug() {
  set +e
  OUT="$(docker run --rm -v "$WORK:/workspace" -w /workspace "$@" "$IMG" 2>&1)"; RC=$?
  set -e
}

# 4. required-input guard (missing dir) still fails closed at the entrypoint.
plug -e NETLIFY_AUTH_TOKEN=x -e PLUGIN_SITE_ID=x
[ "$RC" -eq 2 ] || die "missing dir should exit 2 (rc=$RC)" "$OUT"

# 5. real dir + throwaway creds → the deploy codepath runs and reaches an
#    EXPECTED auth/API failure (non-zero), NOT a broken-module error. This is
#    what proves removing the binary didn't break `netlify deploy`.
plug -e PLUGIN_DIR=site -e PLUGIN_SITE_ID=nonexistent-smoke-site -e NETLIFY_AUTH_TOKEN=invalid-throwaway-token
[ "$RC" -ne 0 ] || die "a deploy with throwaway creds should fail (auth), not succeed" "$OUT"
grep -q "==> netlify deploy" <<<"$OUT" || die "the entrypoint banner did not run — validation failed before the CLI" "$OUT"
# Anchor on the stable auth-failure signature, not just "any non-zero" — a
# network/TLS/init regression must NOT pass as a reached-the-API success. The
# CLI is version-pinned, so if this message ever changes the smoke should fail
# and force a review.
grep -qiE "Unauthorized: could not retrieve project" <<<"$OUT" \
  || die "deploy did not reach the expected auth/API failure (pinned CLI signature changed?)" "$OUT"
if grep -qiE "MODULE_NOT_FOUND|ENOENT|local-functions-proxy|cannot find module" <<<"$OUT"; then
  die "deploy path hit a broken-module/missing-binary error — the removal broke it" "$OUT"
fi

echo "PASS: netlify smoke"
