#!/bin/bash
# gocdnext/gitleaks — thin wrapper around gitleaks. See
# Dockerfile for the full contract.

set -euo pipefail

PATH_TO_SCAN="${PLUGIN_PATH:-.}"
FORMAT="${PLUGIN_FORMAT:-json}"
MODE="${PLUGIN_SCAN_MODE:-dir}"
EXIT_CODE="${PLUGIN_EXIT_CODE:-1}"
VERBOSE="${PLUGIN_VERBOSE:-true}"
REDACT="${PLUGIN_REDACT:-75}"
ALLOWLIST_PATHS_INPUT="${PLUGIN_ALLOWLIST_PATHS:-}"

# dir vs git: `detect` scans files under --source; `git` walks
# commit history. 90% of CI users want the former — "did I just
# commit a secret now?" not "was one ever committed in this
# repo's lifetime?". Keep the gitleaks exit code intact so the
# caller can tune strictness via PLUGIN_EXIT_CODE (0 for
# advisory).
case "${MODE}" in
  dir)
    cmd=(detect --source "${PATH_TO_SCAN}" --no-git)
    ;;
  git)
    cmd=(detect --source "${PATH_TO_SCAN}")
    ;;
  *)
    echo "gocdnext/gitleaks: unknown scan_mode ${MODE} (accepted: dir, git)" >&2
    exit 2
    ;;
esac

cmd+=("--exit-code" "${EXIT_CODE}" "--report-format" "${FORMAT}")

# --verbose prints each finding's file:line + rule + redacted
# secret to stderr as gitleaks discovers them. Without this the
# operator sees only "leaks found: 13" and has to dig through a
# separately-shipped JSON report to find out WHICH files —
# essentially useless as immediate CI feedback. Default on; set
# `verbose: false` to silence (still hits exit-code 1 on findings,
# the JSON report still gets written if --report-path is set).
if [ "${VERBOSE}" != "false" ]; then
  cmd+=("--verbose")
fi

# --redact masks N% of the secret in the verbose output so the
# log shows ENOUGH of the value to confirm "yes that's my AWS
# key" without leaving the key in plaintext in the build log
# stream. 75% redact leaves prefix + suffix visible — typical
# AKIA…XXXX pattern is identifiable; the body is masked. Set
# `redact: 0` to disable (PRINTS THE SECRET in logs — only do
# this in a private project) or `redact: 100` to fully mask.
cmd+=("--redact" "${REDACT}")

# build_allowlist_config materialises a runtime gitleaks config under
# /tmp from the operator's `allowlist-paths:` input. Each token is
# treated as a LITERAL substring (regex meta-chars escaped) and
# becomes a `.*<escaped>.*` entry under `[allowlist].paths` — a
# directory name like `docs/` matches `docs/`, `services/web/docs/`,
# etc. Anchored matching would surprise monorepo users who expect
# "anywhere named docs" semantics.
#
# Composition with operator-supplied `config:`:
#   - Both set: runtime uses `[extend].path = <user's config>` so
#     the user's rules + allowlists chain; our paths are additive.
#   - Only allowlist-paths set: runtime uses `[extend].useDefault =
#     true` so the built-in gitleaks ruleset stays active. Without
#     this, gitleaks would run with ZERO rules — the very thing
#     `config:` already does today and the operator never sees,
#     because the v1 plugin only passed --config when explicitly
#     set. We MUST keep useDefault on for the allowlist-paths-only
#     case to preserve current detection coverage.
#
# Input format: comma or whitespace separated. Empty entries
# silently skipped (`docs/,, tests/` is fine). Path-traversal
# (`..`) and absolute paths (`/etc/...`) rejected at parse —
# operator should not be able to point allowlist outside the
# workspace via this knob.
build_allowlist_config() {
  local input="$1" config_path="$2"
  local runtime_config=/tmp/gocdnext-gitleaks-runtime.toml
  local entries=()

  # Tokenise on commas + whitespace. IFS shenanigans keep this
  # portable across bash 4/5 and don't rely on `mapfile`.
  local raw_token cleaned
  local IFS=$',\n\t '
  for raw_token in ${input}; do
    [ -z "${raw_token}" ] && continue
    cleaned="${raw_token}"

    # Validation. Loud rejection — silent skip would let a typo
    # ("doks/") be interpreted as "no allowlist, scan everything"
    # without warning the operator.
    case "${cleaned}" in
      ..|*/../*|../*|*/..)
        # `..` standalone is rejected too — without that branch
        # the value passes the existing guards (no leading slash,
        # charset ok) but generates the regex `.*\.\..*` once
        # escaped, which is a weird-broad match the operator
        # never asked for. Catch all four traversal shapes here.
        echo "gocdnext/gitleaks: allowlist-paths entry contains '..' (path traversal not allowed)" >&2
        echo "  rejected: '${cleaned}'" >&2
        exit 2
        ;;
      /*)
        echo "gocdnext/gitleaks: allowlist-paths entries must be relative to the workspace" >&2
        echo "  rejected: '${cleaned}' (leading slash)" >&2
        exit 2
        ;;
    esac
    # Charset: alphanumeric, /, _, -, . only. Rejects regex
    # meta-chars (*, +, [], etc.) so a malformed input doesn't
    # land an unintended regex in the runtime config. Operator
    # wanting real regex uses `config: .gitleaks.toml` directly.
    case "${cleaned}" in
      *[!a-zA-Z0-9/_.-]*)
        echo "gocdnext/gitleaks: allowlist-paths entry contains disallowed chars" >&2
        echo "  rejected: '${cleaned}'" >&2
        echo "  allowed: [a-zA-Z0-9/_.-]; use a .gitleaks.toml config for real regex" >&2
        exit 2
        ;;
    esac
    entries+=("${cleaned}")
  done

  if [ "${#entries[@]}" -eq 0 ]; then
    # Operator passed allowlist-paths but every token was empty
    # (e.g. " , , " or trailing comma). Treat as legitimate
    # "no paths to allowlist" — return 0 with empty stdout so the
    # caller skips --config injection. Distinct from a validation
    # error which exits 2 and propagates via `set -e`.
    return 0
  fi

  # Generate the runtime TOML. Triple-quoted strings (TOML literal)
  # so the regex's `.` and `\` survive without escaping. Each
  # operator-provided path becomes `.*<path>.*` — substring match,
  # the predictable choice for monorepo `docs/` and `tests/`.
  {
    if [ -n "${config_path}" ]; then
      # Chain the operator's config — its rules + allowlists stay
      # active; ours append.
      local abs_config
      abs_config="$(readlink -f "${config_path}" 2>/dev/null || printf '%s' "${config_path}")"
      # TOML triple-single-quote literal strings disable ALL escape
      # processing — `\`, `"`, newlines, everything in the path
      # round-trips literally. The ONLY forbidden sequence is the
      # closing delimiter `'''` itself. Reject paths containing
      # it; almost no real filesystem path has three consecutive
      # apostrophes, but a malicious or pathological input could
      # otherwise unmatched-close the literal and inject syntax
      # into the generated config (`'''path'''[rules]''' = …`).
      case "${abs_config}" in
        *\'\'\'*)
          echo "gocdnext/gitleaks: config path contains \"'''\" — refusing to generate TOML config that would unmatched-close the literal string" >&2
          echo "  config: ${config_path}" >&2
          exit 2
          ;;
      esac
      printf "[extend]\npath = '''%s'''\n\n" "${abs_config}"
    else
      # No user config: keep gitleaks default rules so we don't
      # accidentally disable secret detection just because someone
      # asked us to skip docs/.
      printf '[extend]\nuseDefault = true\n\n'
    fi
    printf '[allowlist]\npaths = [\n'
    local p
    for p in "${entries[@]}"; do
      # Escape regex meta in the literal path. The charset check
      # above already restricts to [a-zA-Z0-9/_.-], so the only
      # regex-meaningful chars left are `.`. Escape it.
      local escaped="${p//./\\.}"
      printf "  '''.*%s.*''',\n" "${escaped}"
    done
    printf ']\n'
  } > "${runtime_config}"

  printf '%s' "${runtime_config}"
  return 0
}

# Resolve final config: allowlist-paths takes precedence as the
# wrapper; if neither is set, fall back to gitleaks default rules
# (no --config flag — same as v1 plugin behaviour).
#
# NO `|| true` here. A validation error inside the function calls
# `exit 2`, which inside a `$()` subshell only exits the subshell
# — but `set -e` at the top of this script propagates the non-zero
# exit code from the command substitution and aborts the plugin.
# Suppressing with `|| true` would silence the validation and let
# the scan run as if allowlist-paths had been empty — exactly the
# silent-failure mode the loud rejection was supposed to prevent.
runtime_config=""
if [ -n "${ALLOWLIST_PATHS_INPUT}" ]; then
  runtime_config="$(build_allowlist_config "${ALLOWLIST_PATHS_INPUT}" "${PLUGIN_CONFIG:-}")"
fi

if [ -n "${runtime_config}" ]; then
  cmd+=("--config" "${runtime_config}")
elif [ -n "${PLUGIN_CONFIG:-}" ]; then
  cmd+=("--config" "${PLUGIN_CONFIG}")
fi

if [ -n "${PLUGIN_REPORT:-}" ]; then
  cmd+=("--report-path" "${PLUGIN_REPORT}")
fi

# Git 2.35 dubious-ownership workaround, same as every other
# plugin that touches git metadata.
git config --global --add safe.directory '*' 2>/dev/null || true

echo "==> gitleaks ${cmd[*]}"
exec gitleaks "${cmd[@]}"
