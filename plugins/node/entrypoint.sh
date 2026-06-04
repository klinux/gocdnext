#!/bin/bash
# gocdnext/node v2 — Node.js install + run runner.
#
# v2 is a BREAKING REWRITE of v1. v1 was a thin `pnpm <command>`
# prefixer; v2 mirrors the python plugin's "install + run via shell"
# contract:
#
#   PLUGIN_COMMAND      (optional)  shell command to run AFTER install.
#                                   Empty + install:true = install-only
#                                   job (the artifact uploader / cache
#                                   warmer pattern). Executed via
#                                   `bash -lc` so `&&`, pipes, redirects
#                                   work — NOT prefixed with pnpm/npm.
#   PLUGIN_WORKING_DIR  (optional)  directory under workspace to cd into.
#                                   Default ".".
#   PLUGIN_MANAGER      (optional)  pnpm | npm | yarn | none | auto.
#                                   Default "auto" detects from lockfile
#                                   (pnpm-lock.yaml > yarn.lock >
#                                   package-lock.json). "none" skips
#                                   install + setup, runs command only.
#   PLUGIN_INSTALL      (optional)  bool, default "true". Set "false" to
#                                   skip the install step (downstream
#                                   job consuming a `node_modules/`
#                                   artifact via needs_artifacts).
#   PLUGIN_FROZEN       (optional)  bool, default "true". Maps to:
#                                     pnpm → --frozen-lockfile
#                                     npm  → `npm ci` (vs `npm install`)
#                                     yarn v3+ → --immutable
#   PLUGIN_PROD         (optional)  bool, default "false". Skip dev
#                                   dependencies for production builds.
#                                   Maps to:
#                                     pnpm → --prod
#                                     npm  → --omit=dev
#                                     yarn v3+ → `yarn workspaces focus
#                                                --all --production`
#                                                (requires the
#                                                workspace-tools plugin
#                                                — bundled in Yarn 4,
#                                                opt-in in Yarn 3)
#
# Migration from v1: see CHANGELOG.md for the migration table. Short
# version: `command: install --frozen-lockfile` becomes just
# `# (defaults install:true frozen:true do this automatically)`;
# `command: --filter @web lint` becomes
# `command: pnpm --filter @web lint`.
#
# Yarn v1 is INTENTIONALLY rejected. v1 is in maintenance-only mode
# since 2022; supporting both v1 and v3+ install-flag dialects doubles
# the test matrix for ~zero modern users. Operators on yarn v1 either
# migrate to yarn v3+ or fall back to `manager: none` + a plain script.

set -euo pipefail

# parse_bool returns "true" or "false" on stdout, exits non-zero on
# unparseable input. Strict by design: a typo like "flase" or "treu"
# must fail loud rather than silently flipping the operator's intended
# behaviour. Accepts {true,false,1,0,yes,no} case-insensitive plus an
# empty string (treated as "false" — preserves "input not set" =
# "default behaviour" for callers that pre-set a default).
#
# Why strict (vs the v1 plugin's looser is_truthy): typos are common
# in YAML — `install: flase` would have silently skipped install with
# the loose parser, then the operator would chase a "why no node_modules"
# bug. Loud rejection at plugin start is cheaper than the bug hunt.
parse_bool() {
    local val
    val="$(printf '%s' "${1:-}" | tr '[:upper:]' '[:lower:]')"
    case "${val}" in
        true|1|yes) printf 'true' ;;
        false|0|no|'') printf 'false' ;;
        *) return 1 ;;
    esac
}

# parse_bool_input reads PLUGIN_${1} env, parses with parse_bool, falls
# back to ${2} on empty. Exits 2 with a clear message on bad input so
# the operator sees which input rejected and what's allowed.
parse_bool_input() {
    local name="$1" default="$2" raw out
    raw="$(printf '%s' "${!name:-}")"
    if [ -z "${raw}" ]; then
        printf '%s' "${default}"
        return 0
    fi
    out="$(parse_bool "${raw}")" || {
        echo "gocdnext/node: invalid ${name}=${raw} — accepted: true|false|1|0|yes|no" >&2
        exit 2
    }
    printf '%s' "${out}"
}

# ─── inputs ────────────────────────────────────────────────────────────
COMMAND="${PLUGIN_COMMAND:-}"
WORKING_DIR="${PLUGIN_WORKING_DIR:-.}"
MANAGER_INPUT="$(printf '%s' "${PLUGIN_MANAGER:-auto}" | tr '[:upper:]' '[:lower:]')"
INSTALL="$(parse_bool_input PLUGIN_INSTALL true)"
FROZEN="$(parse_bool_input PLUGIN_FROZEN true)"
PROD="$(parse_bool_input PLUGIN_PROD false)"

cd "${WORKING_DIR}"

# Git 2.35+ "dubious ownership" — host-cloned workspace owned by the
# agent's UID, container runs as root. Same workaround the v1 plugin
# and other plugins apply.
git config --global --add safe.directory '*' 2>/dev/null || true

# ─── manager detection ─────────────────────────────────────────────────
# Auto-detect priority: pnpm > yarn > npm > error. Lockfile presence
# wins over `packageManager:` field in package.json — the lockfile is
# what the developer actually used to resolve dependencies; the field
# is a hint to corepack for which binary to fetch.
#
# Yarn v3+ vs v1 detection via `.yarnrc.yml` (v3+ uses YAML config,
# v1 uses plain `.yarnrc`).
detect_manager() {
    if [ -f pnpm-lock.yaml ]; then echo pnpm; return; fi
    if [ -f yarn.lock ]; then
        if [ -f .yarnrc.yml ]; then echo yarn; return; fi
        echo yarn-v1
        return
    fi
    if [ -f package-lock.json ]; then echo npm; return; fi
    echo unknown
}

MANAGER="${MANAGER_INPUT}"
if [ "${MANAGER}" = "auto" ]; then
    MANAGER="$(detect_manager)"
fi

# detect_manager can return "yarn-v1" as an internal signal; collapse
# to "yarn" + flag so the v1 rejection runs against BOTH the auto-
# detected path AND the explicit `manager: yarn` path (a yarn.lock
# without .yarnrc.yml means v1 regardless of how we got here).
yarn_is_v1=false
case "${MANAGER}" in
    yarn-v1)
        MANAGER=yarn
        yarn_is_v1=true
        ;;
    yarn)
        if [ -f yarn.lock ] && [ ! -f .yarnrc.yml ]; then
            yarn_is_v1=true
        fi
        ;;
esac

if [ "${MANAGER}" = "unknown" ]; then
    echo "gocdnext/node: couldn't detect package manager (no lockfile found)." >&2
    echo "  add pnpm-lock.yaml / yarn.lock / package-lock.json, OR set \`manager:\` explicitly" >&2
    exit 2
fi

# Enum gate AFTER auto-detect resolution but BEFORE setup. Anything
# outside the closed set (`pnpmx`, `Auto` post-lowercase that doesn't
# match `auto`, an empty string from a YAML `manager: ` literal) was
# previously silent — setup case `pnpm|npm|yarn|none)` fell through
# without an `*)` branch, install also skipped silently, the command
# ran with no deps and the run finished green. Closing the gate here
# is the v0.4.39 HIGH fix.
case "${MANAGER}" in
    pnpm|npm|yarn|none) ;;
    *)
        echo "gocdnext/node: invalid manager '${MANAGER_INPUT}'" >&2
        echo "  accepted: pnpm | npm | yarn | none | auto" >&2
        exit 2
        ;;
esac

if [ "${MANAGER}" = "yarn" ] && [ "${yarn_is_v1}" = "true" ]; then
    echo "gocdnext/node: yarn v1 is not supported (maintenance-only upstream)." >&2
    echo "  options:" >&2
    echo "    1. migrate to yarn v3+ (add .yarnrc.yml + run \`yarn set version stable\`)" >&2
    echo "    2. switch to pnpm or npm" >&2
    echo "    3. use \`manager: none\` and run yarn directly via \`command:\`" >&2
    exit 2
fi

# Validate: empty command must come with REAL install work — otherwise
# the job does nothing and exits 0, which v1 reviewers correctly flagged
# as the silent-no-op foot-gun. `install: false` skips install. `manager:
# none` ALSO skips install. Either of those + empty command = silent
# success. Reject both shapes with one message.
if [ -z "${COMMAND}" ]; then
    if [ "${INSTALL}" != "true" ] || [ "${MANAGER}" = "none" ]; then
        echo "gocdnext/node: command:\"\" with no install would be a no-op job" >&2
        echo "  current: install=${INSTALL}, manager=${MANAGER}" >&2
        echo "  set command:<shell command> OR keep install:true with manager!=none" >&2
        exit 2
    fi
fi

echo "==> manager: ${MANAGER}"

# ─── setup ─────────────────────────────────────────────────────────────
# corepack is required for pnpm and yarn (both ship as corepack-managed
# binaries since Node 16.13). npm comes with the node base image and
# doesn't need corepack. `manager: none` skips setup entirely — operator
# is on their own.
case "${MANAGER}" in
    pnpm|yarn)
        corepack enable
        # --activate forces the binary to be fetched now so the first
        # invocation doesn't fail with a stale shim. Reads the
        # `packageManager:` field from package.json to pick the version.
        corepack prepare --activate
        ;;
    npm|none)
        ;;
esac

# Point each manager's cache/store at a workspace-relative dir so the
# platform's `cache:` block can tar it across runs. Default locations
# sit under $HOME, which the agent's tar step can't reach. The cache
# dir is the SAME for every job (deterministic per-manager) so operators
# can reference it in `cache: paths:` without per-job customisation.
case "${MANAGER}" in
    pnpm)
        export PNPM_STORE_DIR="${PNPM_STORE_DIR:-.pnpm-store}"
        mkdir -p "${PNPM_STORE_DIR}"
        pnpm config set store-dir "${PNPM_STORE_DIR}" >/dev/null
        ;;
    npm)
        export NPM_CONFIG_CACHE="${NPM_CONFIG_CACHE:-.npm-cache}"
        mkdir -p "${NPM_CONFIG_CACHE}"
        ;;
    yarn)
        # Yarn v3+ stores under .yarn/cache by default — already
        # workspace-relative. No override needed.
        :
        ;;
esac

# ─── install ───────────────────────────────────────────────────────────
# Build the install command per manager. Each manager has its own
# dialect for "frozen lockfile" and "skip dev deps":
#
#                 frozen=true        frozen=false      prod=true
#   pnpm          --frozen-lockfile  (no flag)         + --prod
#   npm           npm ci             npm install       + --omit=dev
#   yarn v3+      --immutable        (no flag)         workspaces focus
#                                                      --all --production
#                                                      (frozen implied;
#                                                      requires plugin
#                                                      workspace-tools)
#
# `npm ci` IS the frozen variant of npm install — no `--frozen-lockfile`
# flag on npm.
run_install() {
    case "${MANAGER}" in
        pnpm)
            local args=(install)
            if [ "${FROZEN}" = "true" ]; then args+=(--frozen-lockfile); fi
            if [ "${PROD}" = "true" ]; then args+=(--prod); fi
            echo "==> install: pnpm ${args[*]}"
            pnpm "${args[@]}"
            ;;
        npm)
            local args
            if [ "${FROZEN}" = "true" ]; then args=(ci); else args=(install); fi
            if [ "${PROD}" = "true" ]; then args+=(--omit=dev); fi
            echo "==> install: npm ${args[*]}"
            npm "${args[@]}"
            ;;
        yarn)
            # Yarn Modern (v3+) deprecated `yarn install --production`
            # in v2 — it now silently maps to `yarn workspaces focus
            # --all --production` per the official migration guide
            # (https://yarnpkg.com/migration/guide). The mapping has
            # been quiet about behavioural drift for years; calling
            # the focus command directly removes the layer of
            # surprise.
            #
            # `workspaces focus` belongs to the @yarnpkg/plugin-
            # workspace-tools plugin. Yarn 4 bundles it BY DEFAULT;
            # Yarn 3 DOES NOT — the project must add it explicitly
            # via `yarn plugin import workspace-tools` (which writes
            # an entry to .yarnrc.yml + .yarn/plugins/). Without
            # the plugin, yarn errors with "Couldn't find a script
            # named focus" which doesn't hint at the fix. Preflight
            # check below detects the gap and surfaces actionable
            # remediation steps.
            #
            # `focus --all --production` validates the lockfile by
            # design (yarn refuses with non-zero on drift), so the
            # FROZEN flag is structurally implied in this branch.
            # Documenting that here rather than failing on
            # FROZEN=false would surprise the operator who set both
            # — instead we just always behave frozen for PROD=true.
            local args
            if [ "${PROD}" = "true" ]; then
                if ! yarn workspaces focus --help >/dev/null 2>&1; then
                    echo "gocdnext/node: prod:true + manager:yarn requires the workspace-tools plugin." >&2
                    echo "  This plugin is bundled in Yarn 4 by default; Yarn 3 must opt in." >&2
                    echo "  yarn version: $(yarn -v 2>/dev/null || echo 'unknown')" >&2
                    echo "  Fix (commit to repo):" >&2
                    echo "    yarn plugin import workspace-tools" >&2
                    echo "    git add .yarnrc.yml .yarn/plugins/" >&2
                    echo "  OR upgrade to Yarn 4: yarn set version stable" >&2
                    exit 2
                fi
                args=(workspaces focus --all --production)
            else
                args=(install)
                if [ "${FROZEN}" = "true" ]; then args+=(--immutable); fi
            fi
            echo "==> install: yarn ${args[*]}"
            yarn "${args[@]}"
            ;;
        none)
            echo "==> install: skipped (manager: none)"
            ;;
        *)
            # Belt-and-braces: enum was validated above, this branch
            # is unreachable in practice. Fail loud if a future
            # refactor adds a manager to the validation but forgets
            # to add the install dialect here.
            echo "gocdnext/node: internal — run_install reached for unknown manager '${MANAGER}'" >&2
            exit 1
            ;;
    esac
}

if [ "${INSTALL}" = "true" ] && [ "${MANAGER}" != "none" ]; then
    run_install
elif [ "${INSTALL}" = "false" ]; then
    echo "==> install: skipped (install: false — assuming node_modules restored via artifact)"
fi

# ─── command ───────────────────────────────────────────────────────────
# Empty command + install ran = install-only job (artifact uploader,
# cache warmer). Exit clean.
if [ -z "${COMMAND}" ]; then
    echo "==> command: (empty, install-only job)"
    exit 0
fi

# `bash -lc` so `&&`, pipes, redirects, env expansion all work as the
# operator wrote them. -l reads /etc/profile + ~/.bash_profile which
# corepack writes into so its shim PATH propagates.
echo "==> command: ${COMMAND}"
exec bash -lc -- "${COMMAND}"
