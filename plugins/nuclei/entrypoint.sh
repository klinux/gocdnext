#!/bin/bash
# gocdnext/nuclei — DAST baseline scanner (ProjectDiscovery Nuclei). See the
# Dockerfile for the full PLUGIN_* input contract.
#
# Security posture (v1 = unauthenticated baseline + API-spec-driven):
#   - no auth/header input (no secret on argv);
#   - OAST off by default (-no-interactsh) unless explicitly opted in;
#   - HTTP redirects off (-disable-redirects) so the scan can't leave the target;
#   - scoped to HTTP templates (-type http);
#   - templates baked in the image, never updated at runtime (-disable-update-check);
#   - a spec's base URL is rewritten to PLUGIN_TARGET so an OpenAPI/Swagger
#     pointing at prod can't redirect the scan;
#   - preflight refuses to record a false "clean" scan against a target that
#     never came up;
#   - a real nuclei error is exit 2, never a synthesized clean SARIF.
set -euo pipefail

TARGET="${PLUGIN_TARGET:-}"
SEVERITY="${PLUGIN_SEVERITY:-critical,high,medium}"
FAIL_ON="${PLUGIN_FAIL_ON:-critical,high}"
EXIT_CODE="${PLUGIN_EXIT_CODE:-1}"
SPEC="${PLUGIN_SPEC:-}"
SPEC_FORMAT="${PLUGIN_SPEC_FORMAT:-openapi}"
TAGS="${PLUGIN_TAGS:-}"
TEMPLATES="${PLUGIN_TEMPLATES:-}"
READY_TIMEOUT="${PLUGIN_READY_TIMEOUT:-60}"
HEALTH_PATH="${PLUGIN_HEALTH_PATH:-}"
RATE_LIMIT="${PLUGIN_RATE_LIMIT:-50}"
CONCURRENCY="${PLUGIN_CONCURRENCY:-}"
TIMEOUT="${PLUGIN_TIMEOUT:-10}"
INTERACTSH="${PLUGIN_INTERACTSH:-false}"
INTERACTSH_SERVER="${PLUGIN_INTERACTSH_SERVER:-}"
OUTPUT="${PLUGIN_OUTPUT:-nuclei.sarif}"
TEMPLATES_DIR="${NUCLEI_TEMPLATES_DIR:-/opt/nuclei-templates}"

die() { echo "gocdnext/nuclei: $*" >&2; exit 2; }

[ -n "${TARGET}" ] || die "PLUGIN_TARGET is required (the base URL to scan, e.g. http://app:8080)"

# ── input validation (fail loud, never silently misbehave) ──────────────────
is_uint() { [[ "${1}" =~ ^[0-9]+$ ]] && [ "${1}" -gt 0 ]; }
is_uint "${READY_TIMEOUT}" || die "ready-timeout must be a positive integer (got '${READY_TIMEOUT}')"
is_uint "${RATE_LIMIT}"    || die "rate-limit must be a positive integer (got '${RATE_LIMIT}')"
is_uint "${TIMEOUT}"       || die "timeout must be a positive integer (got '${TIMEOUT}')"
[ -z "${CONCURRENCY}" ] || is_uint "${CONCURRENCY}" || die "concurrency must be a positive integer (got '${CONCURRENCY}')"
case "${EXIT_CODE}" in 0|1|2) ;; *) die "exit-code must be 0, 1 or 2 (got '${EXIT_CODE}')" ;; esac

# interactsh-server only makes sense with OAST enabled — don't silently ignore.
if [ -n "${INTERACTSH_SERVER}" ] && [ "${INTERACTSH}" != "true" ]; then
  die "interactsh-server is set but interactsh is not true; set interactsh: true to enable OAST, or remove interactsh-server"
fi

# fail-on must be a subset of severity, else it can never fire.
IFS=',' read -ra SEV_ARR <<< "${SEVERITY}"
IFS=',' read -ra FAIL_ARR <<< "${FAIL_ON}"
for f in "${FAIL_ARR[@]}"; do
  found=0
  for s in "${SEV_ARR[@]}"; do [ "${f}" = "${s}" ] && found=1 && break; done
  [ "${found}" = 1 ] || die "fail-on cannot include severities excluded by severity ('${f}' not in '${SEVERITY}'); add them to severity or lower fail-on"
done

# ── preflight: the target must be up, else we do NOT record a clean scan ─────
preflight_url="${TARGET}"
if [ -n "${HEALTH_PATH}" ]; then
  preflight_url="${TARGET%/}/${HEALTH_PATH#/}"   # clean join, no // and no concat
fi
echo "==> waiting for target ${preflight_url} (timeout ${READY_TIMEOUT}s)"
deadline=$(( SECONDS + READY_TIMEOUT ))
ready=0
code=000
while [ "${SECONDS}" -lt "${deadline}" ]; do
  code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "${preflight_url}" 2>/dev/null || echo 000)"
  if [ -n "${HEALTH_PATH}" ]; then
    case "${code}" in 2??|3??) ready=1; break ;; esac      # health-path → require 2xx/3xx
  else
    [ "${code}" != "000" ] && { ready=1; break; }          # root → any HTTP reply = alive
  fi
  sleep 2
done
[ "${ready}" = 1 ] || die "target ${preflight_url} not reachable within ${READY_TIMEOUT}s (last code ${code}) — refusing to record a false clean scan"

TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT
JSONL="${TMP}/findings.jsonl"

# ── build the nuclei argv (hardened defaults) ───────────────────────────────
args=(
  -severity "${SEVERITY}"
  -sarif-export "${OUTPUT}"
  -jsonl-export "${JSONL}"
  -omit-raw
  -redact "authorization,cookie,set-cookie,x-api-key"
  -disable-update-check
  -disable-redirects
  -type http
  -rate-limit "${RATE_LIMIT}"
  -timeout "${TIMEOUT}"
)
[ -n "${CONCURRENCY}" ] && args+=( -concurrency "${CONCURRENCY}" )
[ -n "${TAGS}" ] && args+=( -tags "${TAGS}" )
if [ -n "${TEMPLATES}" ]; then
  args+=( -t "${TEMPLATES}" )           # explicit override replaces the baked set
else
  args+=( -t "${TEMPLATES_DIR}" )       # authoritative CLI path to the baked set
fi
if [ "${INTERACTSH}" = "true" ]; then
  [ -n "${INTERACTSH_SERVER}" ] && args+=( -interactsh-server "${INTERACTSH_SERVER}" )
else
  args+=( -no-interactsh )
fi

# Spec mode: rewrite the spec's base URL to TARGET (a copy) so it can't redirect
# the scan to prod, then point nuclei at the copy via -list + the matching -im.
if [ -n "${SPEC}" ]; then
  [ -f "${SPEC}" ] || die "spec file not found: ${SPEC}"
  speccopy="${TMP}/$(basename "${SPEC}")"
  cp "${SPEC}" "${speccopy}"
  fmt="${SPEC_FORMAT}"
  if yq -e '.openapi' "${speccopy}" >/dev/null 2>&1; then
    fmt=openapi
  elif yq -e '.swagger' "${speccopy}" >/dev/null 2>&1; then
    fmt=swagger
  fi
  case "${fmt}" in
    openapi)
      TARGET="${TARGET}" yq -i '.servers = [{"url": strenv(TARGET)}]' "${speccopy}"
      ;;
    swagger)
      scheme="${TARGET%%://*}"
      rest="${TARGET#*://}"
      hostport="${rest%%/*}"
      if [ "${rest}" = "${hostport}" ]; then basepath="/"; else basepath="/${rest#*/}"; fi
      SCHEME="${scheme}" HOSTPORT="${hostport}" BASEPATH="${basepath}" \
        yq -i '.schemes = [strenv(SCHEME)] | .host = strenv(HOSTPORT) | .basePath = strenv(BASEPATH)' "${speccopy}"
      ;;
    *)
      die "unknown spec-format '${fmt}' (expected openapi or swagger)"
      ;;
  esac
  args+=( -list "${speccopy}" -im "${fmt}" )
  echo "==> nuclei (spec ${fmt}) against ${TARGET}"
else
  args+=( -u "${TARGET}" )
  echo "==> nuclei baseline against ${TARGET}"
fi

# ── run; a non-zero exit is an operational error, NOT findings ───────────────
set +e
nuclei "${args[@]}"
rc=$?
set -e
if [ "${rc}" -ne 0 ]; then
  die "nuclei exited ${rc} (operational error — config/template/spec/network); not recording a scan"
fi

# Valid scan → guarantee a parser-valid SARIF even with zero findings, so the
# Security dashboard records a *clean* DAST scan (clean ≠ not-scanned).
if [ ! -s "${OUTPUT}" ]; then
  cat > "${OUTPUT}" <<'SARIF'
{"version":"2.1.0","$schema":"https://json.schemastore.org/sarif-2.1.0.json","runs":[{"tool":{"driver":{"name":"nuclei"}},"results":[]}]}
SARIF
fi

# ── fail-on gate (our gate is the JSONL, not nuclei's exit) ──────────────────
matched=0
if [ -s "${JSONL}" ]; then           # absent/empty on a clean run = 0 findings
  while IFS= read -r sev; do
    for f in "${FAIL_ARR[@]}"; do [ "${sev}" = "${f}" ] && matched=$((matched + 1)) && break; done
  done < <(jq -r '.info.severity // empty' "${JSONL}")
fi
echo "==> nuclei: ${matched} finding(s) at/above fail-on (${FAIL_ON})"
if [ "${matched}" -gt 0 ]; then
  exit "${EXIT_CODE}"
fi
exit 0
