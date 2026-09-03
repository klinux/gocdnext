#!/bin/bash
# select-node.sh — pick the Node.js major and point PATH at it, the
# same way jdk-base/select-jdk.sh picks a JDK. Sourced by the node
# plugin entrypoint BEFORE any package manager runs:
#
#     . /usr/local/bin/select-node.sh
#
# The image bakes several node majors under ${NODE_BASE_DIR}/node-<major>
# (default /opt/node). Selection is a pure PATH switch — NO download —
# so a node-24 project runs on node 24 with zero runtime cost.
#
# Version source, in precedence order:
#   1. PLUGIN_NODE_VERSION  — the operator's explicit `node-version:` input.
#   2. package.json `engines.node`  — the project's own declaration
#      (mirrors how the dotnet plugin reads global.json).
#   3. .node-version / .nvmrc.
#   4. Default (see DEFAULT_NODE_MAJOR).
#
# An UNBAKED or unparseable version fails LOUD (exit 2) — never a silent
# fall-back to the default, which would run the build on a node the
# project didn't ask for (the silent-skew foot-gun). An explicit input
# that disagrees with engines.node is honored (override is a legitimate
# knob) but printed so the divergence is visible, not hidden.
#
# NODE_BASE_DIR is overridable so the test harness can inject a temp
# tree of fake node-<major>/bin/node stubs.

NODE_BASE_DIR="${NODE_BASE_DIR:-/opt/node}"
DEFAULT_NODE_MAJOR="${DEFAULT_NODE_MAJOR:-22}"

# _node_major_from_range extracts the major integer from a semver-ish
# string: "24.x" / "^24" / ">=24 <25" / "24.0.0" / "v24" → 24. Bounded
# character-class match (no eval, no ReDoS); takes the FIRST integer,
# which is the lower bound for a range — the conservative choice.
_node_major_from_range() {
    # `|| true`: a value with no digit (e.g. "lts/*") makes grep exit 1;
    # under the entrypoint's `set -e` that would abort — we want an empty
    # result the caller turns into a loud "couldn't parse", not a crash.
    printf '%s' "${1:-}" | grep -oE '[0-9]+' | head -1 || true
}

# _baked_majors lists the node majors baked into the image, ascending.
_baked_majors() {
    (cd "${NODE_BASE_DIR}" 2>/dev/null && ls -d node-* 2>/dev/null) \
        | sed 's/^node-//' | sort -n || true
}

# _resolve_engines_major maps an engines.node range to the SMALLEST baked
# major whose FULL version (X.Y.Z) actually satisfies the range — using
# the REAL semver bundled in the image's npm, so minor/patch bounds
# (">=20.99.0"), operator spaces (">= 18") and hyphen ranges ("18 - 22")
# are honored exactly, not approximated. Returns:
#   - a baked major   → satisfiable, use it;
#   - "UNSAT"         → valid range, but NO baked version satisfies it
#                       (e.g. "<20", ">24", ">=24.99.0") — caller fails loud;
#   - "BADRANGE"      → not a valid semver range — caller fails loud;
#   - "NOSEMVER"      → semver isn't available AND the value isn't a plain
#                       wildcard-major we can resolve without it (only the
#                       mock harness, which has no real node/semver, hits
#                       this) — caller fails loud;
#   - ""  (empty)     → no numeric major at all (alias/`*`) — caller falls
#                       through to the next source (default).
_resolve_engines_major() {
    local range="$1" lower node_bin semver_dir baked ver
    lower="$(_node_major_from_range "${range}")"
    [ -z "${lower}" ] && return 0

    node_bin="${NODE_BASE_DIR}/node-${DEFAULT_NODE_MAJOR}/bin/node"
    semver_dir="${NODE_BASE_DIR}/node-${DEFAULT_NODE_MAJOR}/lib/node_modules/npm/node_modules/semver"

    if [ -x "${node_bin}" ] && [ -f "${semver_dir}/package.json" ]; then
        if ! "${node_bin}" -e 'process.exit(require(process.argv[1]).validRange(process.argv[2])?0:1)' \
                "${semver_dir}" "${range}" 2>/dev/null; then
            printf 'BADRANGE'; return 0
        fi
        for baked in $(_baked_majors); do
            ver="$("${NODE_BASE_DIR}/node-${baked}/bin/node" --version 2>/dev/null | sed 's/^v//')"
            [ -z "${ver}" ] && continue
            if "${node_bin}" -e 'process.exit(require(process.argv[1]).satisfies(process.argv[2],process.argv[3])?0:1)' \
                    "${semver_dir}" "${ver}" "${range}" 2>/dev/null; then
                printf '%s' "${baked}"; return 0
            fi
        done
        printf 'UNSAT'; return 0
    fi

    # No real semver (the mock harness). Resolve ONLY a plain wildcard
    # major (`24` / `24.x`) without it; anything with operators, spaces,
    # or explicit minor/patch needs semver → NOSEMVER (caller fails loud).
    case "${range}" in
        *[!0-9.xX*]*) printf 'NOSEMVER'; return 0 ;;
    esac
    if printf '%s' "${range}" | grep -qE '^[0-9]+(\.[xX*])?$'; then
        for baked in $(_baked_majors); do
            [ "${baked}" = "${lower}" ] && { printf '%s' "${lower}"; return 0; }
        done
        printf 'UNSAT'; return 0
    fi
    printf 'NOSEMVER'
}

# _read_engines_node pulls engines.node out of package.json WITHOUT a
# JSON dependency. It flattens newlines, isolates the "engines" object
# (bounded [^}]* — engines has no nested braces), then reads the "node"
# value inside it. Scoping to the engines block avoids matching a
# stray "node" key elsewhere (e.g. an @types/node dependency line).
_read_engines_node() {
    [ -f package.json ] || return 0
    # `|| true`: no engines / no node key makes a grep exit 1, which
    # under `set -e` (this file is SOURCED into the entrypoint) would
    # abort the whole run. An absent value is normal — fall through to
    # the next source instead.
    tr -d '\n' < package.json \
        | grep -oE '"engines"[[:space:]]*:[[:space:]]*\{[^}]*\}' \
        | grep -oE '"node"[[:space:]]*:[[:space:]]*"[^"]*"' \
        | head -1 \
        | sed -E 's/.*"node"[[:space:]]*:[[:space:]]*"([^"]*)".*/\1/' || true
}

# _read_node_version_file reads .node-version or .nvmrc (first that
# exists). Content is a bare version ("20" or "v20.11.0").
_read_node_version_file() {
    local f
    for f in .node-version .nvmrc; do
        if [ -f "$f" ]; then
            head -1 "$f" | tr -d '[:space:]'
            return 0
        fi
    done
    return 0
}

_select_node() {
    local explicit engines_raw file_raw major source declared_major

    explicit="$(printf '%s' "${PLUGIN_NODE_VERSION:-}" | tr -d '[:space:]')"
    engines_raw="$(_read_engines_node)"
    # Resolve against the baked majors so an open range like ">=18" widens
    # to the smallest baked major (20) instead of failing on 18.
    declared_major="$(_resolve_engines_major "${engines_raw}")"

    if [ -n "${explicit}" ] && [ "${explicit}" != "auto" ]; then
        # An EXPLICIT input is intent: garbage (no numeric major) fails
        # loud — the operator asked for something we can't parse.
        major="$(_node_major_from_range "${explicit}")"
        if [ -z "${major}" ]; then
            echo "gocdnext/node: node-version:${explicit} has no numeric major (use 20 | 22 | 24)" >&2
            exit 2
        fi
        source="node-version input"
        # Visible divergence — honored, not hidden (unlike a silent skew).
        # Only when engines resolved to a concrete baked major (numeric).
        if printf '%s' "${declared_major}" | grep -qE '^[0-9]+$' && [ "${declared_major}" != "${major}" ]; then
            echo "gocdnext/node: node-version:${explicit} overrides engines.node (${engines_raw} → ${declared_major})" >&2
        fi
    elif [ "${declared_major}" = "BADRANGE" ]; then
        echo "gocdnext/node: engines.node \"${engines_raw}\" is not a valid semver range" >&2
        echo "  fix it, pin node-version: 20|22|24, or run the job on a custom image:" >&2
        exit 2
    elif [ -n "${declared_major}" ] && ! printf '%s' "${declared_major}" | grep -qE '^[0-9]+$'; then
        # A sentinel (UNSAT / NOSEMVER): engines.node declared a range no
        # baked version satisfies (or one we can't evaluate). Fail loud —
        # NEVER fall back to a major the project didn't ask for.
        echo "gocdnext/node: engines.node \"${engines_raw}\" can't be satisfied by a baked node major ($(_baked_majors | tr '\n' ' '))" >&2
        echo "  it declares a version/range outside the baked set (or a minor/patch we don't ship)" >&2
        echo "  pin node-version: 20|22|24, or run the job on a custom image: with the exact node" >&2
        exit 2
    elif [ -n "${declared_major}" ]; then
        major="${declared_major}"
        source="engines.node (${engines_raw})"
    else
        # A DECLARATIVE hint (engines.node / .nvmrc) that has no numeric
        # major — an `lts/*` alias, `*`, empty — is NOT an error: fall
        # through to the default. Only an explicit input garbage fails
        # (above). Consistent across both hint sources.
        file_raw="$(_read_node_version_file)"
        major="$(_node_major_from_range "${file_raw}")"
        if [ -n "${major}" ]; then
            source=".node-version/.nvmrc (${file_raw})"
        else
            major="${DEFAULT_NODE_MAJOR}"
            source="default"
        fi
    fi

    if [ ! -x "${NODE_BASE_DIR}/node-${major}/bin/node" ]; then
        echo "gocdnext/node: node ${major} (from ${source}) is not baked into this image" >&2
        echo "  baked majors: $(cd "${NODE_BASE_DIR}" 2>/dev/null && ls -d node-* 2>/dev/null | sed 's/node-/ /g' | tr -d '\n' || echo ' none')" >&2
        echo "  pin a supported major (node-version: 20|22|24) OR run the job on a custom image:" >&2
        exit 2
    fi

    # Rebuild PATH dropping any prior ${NODE_BASE_DIR}/node-*/bin so
    # re-sourcing doesn't accrete duplicates. Glob-match in `case`
    # avoids sed-escaping a dynamic NODE_BASE_DIR.
    local _new_path="" _p
    local _oldifs="${IFS}"
    IFS=':'
    for _p in ${PATH}; do
        case "${_p}" in
            "${NODE_BASE_DIR}"/node-*/bin) ;;
            *) _new_path="${_new_path:+${_new_path}:}${_p}" ;;
        esac
    done
    IFS="${_oldifs}"

    export PATH="${NODE_BASE_DIR}/node-${major}/bin:${_new_path}"

    # The entrypoint runs the user's command via `bash -lc`, and a login
    # shell's /etc/profile RESETS PATH to the system default — which would
    # drop the selected node (it lives at /opt/node, off the standard
    # PATH) AND the corepack shims that install into its bin dir. Persist
    # the choice to /etc/profile.d so the login shell re-adds it AFTER
    # /etc/profile sets the default. Best-effort: a read-only /etc isn't
    # fatal (install already runs in this shell, which has the right PATH).
    if [ -d /etc/profile.d ] && [ -w /etc/profile.d ]; then
        printf 'export PATH="%s/node-%s/bin:${PATH}"\n' \
            "${NODE_BASE_DIR}" "${major}" \
            > /etc/profile.d/00-gocdnext-node.sh 2>/dev/null || true
    fi

    echo "==> node: ${major} (${source})"
}

_select_node
