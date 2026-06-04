#!/bin/bash
# gocdnext/python — install deps with the detected manager, then
# run the user's command. See Dockerfile for the full contract.

set -euo pipefail

if [ -z "${PLUGIN_COMMAND:-}" ]; then
    echo "gocdnext/python: PLUGIN_COMMAND is required" >&2
    echo "  example: command: pytest -q" >&2
    exit 2
fi

WORKING_DIR="${PLUGIN_WORKING_DIR:-.}"
MANAGER="${PLUGIN_MANAGER:-auto}"

# Issue #2 inputs.
#
# `extras` is comma/space-separated (same shape every other plugin
# uses for list inputs — see buildx's TAGS handling). Translation
# per-manager lives in the case statement below; here we just
# tokenize once. Empty input → empty array, the case handlers skip
# their `--extra` flags.
#
# `all-extras` and `no-install` are bools — accept "1", "true", "yes"
# (case-insensitive) as truthy, anything else as false. Matches how
# the rest of the gocdnext plugin set parses boolean inputs.
EXTRAS_RAW="${PLUGIN_EXTRAS:-}"
EXTRAS_RAW="${EXTRAS_RAW//,/ }"
read -ra EXTRAS <<<"${EXTRAS_RAW}"

is_truthy() {
    case "${1,,}" in
        1|true|yes|y|on) return 0 ;;
        *) return 1 ;;
    esac
}
ALL_EXTRAS=false
NO_INSTALL=false
if is_truthy "${PLUGIN_ALL_EXTRAS:-false}"; then ALL_EXTRAS=true; fi
if is_truthy "${PLUGIN_NO_INSTALL:-false}"; then NO_INSTALL=true; fi

# all-extras + extras together is ambiguous — all-extras already
# covers the named ones. Refuse the combo so the user sees the
# conflict instead of one silently winning.
if [ "${ALL_EXTRAS}" = "true" ] && [ "${#EXTRAS[@]}" -gt 0 ]; then
    echo "gocdnext/python: 'all-extras: true' and 'extras: [...]' are mutually exclusive" >&2
    echo "  drop one — all-extras already includes everything in extras" >&2
    exit 2
fi

cd "${WORKING_DIR}"

# rewrite_venv_shebangs makes a .venv that arrived via artefact
# usable in this job. Python entry-point scripts (.venv/bin/ruff,
# /mypy, /alembic, …) carry references to the install-job's
# workspace path baked in at install time:
#
#  1. Classic shebang on line 1:
#       `#!/workspace/<old-uuid>/.../.venv/bin/python`
#  2. uv's "exec wrapper" trick on line 2:
#       `#!/bin/sh`
#       `'''exec' "/workspace/<old-uuid>/.../.venv/bin/python3" "$0" "$@"`
#
# A line-1-only shebang rewrite (pre-v0.4.38) caught (1) but
# missed (2), so `uv run mypy app/` would fail with
#   `.venv/bin/mypy: 2: exec: /old/path/python3: not found`
# even after the plugin claimed to have rewritten the venv.
#
# Fix: discover the OLD venv root by reading
# `export VIRTUAL_ENV=...` out of the activate script (every
# manager writes it verbatim at create time — uv, poetry,
# python -m venv all match), then `sed -i 's|old|new|g'` across
# every regular file under .venv/bin. That catches both forms +
# any other absolute reference the wrappers carry. Idempotent —
# the substitution is a no-op when old == new (or when an old
# rewrite already happened). Also rewrite the activate scripts
# themselves so a downstream `source .venv/bin/activate`
# (uncommon but legal) doesn't re-export the dead path.
# `uv venv --relocatable` (tried in v0.4.29) only makes activate
# portable; the entry-point bodies still carry the absolute path.
# Fast: a typical venv has ~30 scripts, each a few KiB.
# activate_venv replaces `source .venv/bin/activate` so we don't
# inherit the install-time hardcoded paths from the upstream
# activate script. uv (and python -m venv) write activate with
# the venv's absolute path baked in:
#   VIRTUAL_ENV="/install-job/path/.venv"
#   PATH="$VIRTUAL_ENV/bin:$PATH"
# When a downstream job sources that, PATH ends up prefixed with
# a nonexistent directory, and `uv run X` resolves X via
# $VIRTUAL_ENV/bin/X — ENOENT, "Failed to spawn X". The mismatch
# warning uv prints is the visible symptom; the ENOENT is the
# actual failure.
#
# We do exactly the same env mutations the upstream activate
# would, just with the CURRENT $PWD/.venv path. Idempotent; cheap.
activate_venv() {
    local venv
    venv="$(pwd)/.venv"
    export VIRTUAL_ENV="${venv}"
    export PATH="${venv}/bin:${PATH}"
    # Standard activate also unsets PYTHONHOME so the venv's
    # interpreter isn't poisoned by an outer python install
    # leaking via PYTHONHOME. Match that behaviour for parity.
    unset PYTHONHOME
}

rewrite_venv_shebangs() {
    [ -d .venv/bin ] || return 0
    local new_venv="$(pwd)/.venv"
    local new_python="${new_venv}/bin/python"
    [ -e "$new_python" ] || return 0

    # Discover the OLD venv root from the activate script. Every
    # manager (uv / poetry / `python -m venv`) writes
    # `export VIRTUAL_ENV=/path/to/.venv` at the same place; we
    # extract that path and use it as the search prefix for the
    # global rewrite below. When activate is missing (a manager
    # we don't know about, or a stripped venv), we fall back to
    # line-1 shebang rewrite only — better than nothing, and
    # covers the classic-shebang case which is the dominant one
    # on pip/poetry venvs.
    local old_venv=""
    if [ -f .venv/bin/activate ]; then
        old_venv="$(awk -F= '
            /^[[:space:]]*export[[:space:]]+VIRTUAL_ENV[[:space:]]*=/ {
                sub(/^[[:space:]]*"/, "", $2);
                sub(/"[[:space:]]*$/, "", $2);
                print $2;
                exit
            }' .venv/bin/activate)"
    fi

    local f line
    for f in .venv/bin/*; do
        [ -f "$f" ] || continue
        # Only touch text files with a shebang. `IFS= read` fails
        # on binaries with NULs in the first line, which skips them.
        IFS= read -r line < "$f" 2>/dev/null || continue
        case "$line" in
            "#!"*)
                # Line 1: rewrite a python-pointing shebang to our
                # interpreter directly. Catches the classic case
                # (`#!/old/.venv/bin/python`) without needing
                # old_venv to be known.
                case "$line" in
                    *python*)
                        sed -i "1c\\#!$new_python" "$f"
                        ;;
                esac
                # Anywhere in the file: substitute the old venv
                # root with the current one. Catches uv's line-2
                # `'''exec' "/old/.venv/bin/python3"` wrapper plus
                # any other absolute reference (e.g. paths in
                # __pycache__ comments). Skipped when old == new
                # (idempotent) or when we couldn't read activate.
                if [ -n "$old_venv" ] && [ "$old_venv" != "$new_venv" ]; then
                    # `|` as the sed delimiter so the / in paths
                    # doesn't need escaping. UUIDs + workspace
                    # paths are alphanumeric + - + /, all safe.
                    sed -i "s|${old_venv}|${new_venv}|g" "$f"
                fi
                ;;
        esac
    done

    # Rewrite the activate variants themselves so a downstream
    # `source .venv/bin/activate` doesn't re-export the dead path
    # and shadow our manual activate_venv() call. Each variant
    # uses the same VIRTUAL_ENV= idiom in its shell syntax; a
    # global path substitution covers all of them.
    if [ -n "$old_venv" ] && [ "$old_venv" != "$new_venv" ]; then
        for f in .venv/bin/activate .venv/bin/activate.csh \
                 .venv/bin/activate.fish .venv/bin/activate.ps1 \
                 .venv/bin/Activate.ps1 .venv/bin/activate.bat; do
            [ -f "$f" ] || continue
            sed -i "s|${old_venv}|${new_venv}|g" "$f"
        done
    fi
}

# Point every package manager's cache dir at a relative-to-PWD
# path so the platform's `cache:` block can tar it. pip/uv/poetry
# each read different env vars; we set them all defensively so
# the user doesn't have to know which manager they're on today.
# `pip install --no-cache-dir` in the pip branch below overrides
# this deliberately — the venv itself becomes the artefact worth
# caching (ephemeral .venv/), not the pip download cache.
# Set AFTER cd so caches sit next to the project, not next to
# the monorepo root — matches what `cache: { path: .cache/pip }`
# would persist when WORKING_DIR is a sub-directory.
export PIP_CACHE_DIR="${PIP_CACHE_DIR:-.cache/pip}"
export UV_CACHE_DIR="${UV_CACHE_DIR:-.cache/uv}"
export POETRY_CACHE_DIR="${POETRY_CACHE_DIR:-.cache/poetry}"
mkdir -p "${PIP_CACHE_DIR}" "${UV_CACHE_DIR}" "${POETRY_CACHE_DIR}"

# Auto-detection priority:
#   poetry.lock    → poetry (library projects, pinned resolver)
#   uv.lock        → uv (fast, modern)
#   requirements.txt → pip (legacy/simple)
#   pyproject.toml → uv (no lockfile yet, but modern project)
#   else → none (user manages venv themselves)
detect_manager() {
    if [ -f "poetry.lock" ]; then echo poetry; return; fi
    if [ -f "uv.lock" ]; then echo uv; return; fi
    if [ -f "requirements.txt" ]; then echo pip; return; fi
    if [ -f "pyproject.toml" ]; then echo uv; return; fi
    echo none
}

if [ "${MANAGER}" = "auto" ]; then
    MANAGER="$(detect_manager)"
fi

echo "==> manager: ${MANAGER}"

# no-install short-circuit. Lets a downstream job consume a `.venv/`
# restored via `needs_artifacts:` from an upstream install job
# without paying the resolve cost again — AND without the original
# bug: the upstream install used `--all-extras`, but a vanilla
# `uv sync --frozen` here would uninstall everything not in the
# base lockfile (ruff/mypy/pytest), then the user's `uv run ruff …`
# would ENOENT-fail because the entry-point script was just deleted.
#
# We deliberately still call rewrite_venv_shebangs + activate_venv
# — those are what make the cross-job .venv usable (the install-job's
# absolute path baked into both the shebangs and the activate
# script). The install step itself is the only piece skipped.
#
# Manager-agnostic: works on whichever upstream chose uv/poetry/pip
# because it touches none of them.
if [ "${NO_INSTALL}" = "true" ]; then
    echo "==> no-install: true — skipping dependency sync, reusing existing .venv"
    if [ ! -d ".venv" ]; then
        echo "gocdnext/python: no-install: true requires an existing .venv (none found at ${WORKING_DIR}/.venv)" >&2
        echo "  add the upstream install job's '.venv/' to needs_artifacts" >&2
        exit 2
    fi
    rewrite_venv_shebangs
    activate_venv
    exec bash -lc -- "${PLUGIN_COMMAND}"
fi

case "${MANAGER}" in
    poetry)
        poetry config virtualenvs.in-project true
        # poetry takes a single --extras with space-separated names,
        # OR --all-extras. The bash array tokens come from EXTRAS;
        # joining with IFS=' ' gives poetry the format it wants.
        poetry_args=(--no-interaction)
        if [ "${ALL_EXTRAS}" = "true" ]; then
            poetry_args+=(--all-extras)
        elif [ "${#EXTRAS[@]}" -gt 0 ]; then
            poetry_args+=(--extras "${EXTRAS[*]}")
        fi
        poetry install "${poetry_args[@]}"
        rewrite_venv_shebangs
        activate_venv
        exec bash -lc -- "${PLUGIN_COMMAND}"
        ;;
    uv)
        # uv takes repeated --extra X flags, OR --all-extras. Build
        # the args explicitly so the lockfile-present branch and the
        # lockfile-absent branch share the same extras handling.
        uv_extras=()
        if [ "${ALL_EXTRAS}" = "true" ]; then
            uv_extras+=(--all-extras)
        else
            for e in "${EXTRAS[@]}"; do
                uv_extras+=(--extra "$e")
            done
        fi
        if [ -f "uv.lock" ]; then
            uv sync --frozen "${uv_extras[@]}"
        else
            uv sync "${uv_extras[@]}"
        fi
        rewrite_venv_shebangs
        activate_venv
        exec bash -lc -- "${PLUGIN_COMMAND}"
        ;;
    pip)
        req="${PLUGIN_REQUIREMENTS:-requirements.txt}"
        if [ ! -f "${req}" ]; then
            echo "gocdnext/python: pip manager selected but ${req} not found" >&2
            exit 2
        fi
        python -m venv .venv
        activate_venv
        # pip's cache dir default was redirected to
        # .cache/pip at the top of this script so a
        # pipeline `cache:` block can preserve the wheel cache
        # across runs — skip --no-cache-dir here.
        pip install -r "${req}"
        # pip has no project-wide --all-extras. Extras only attach
        # to a specific package spec, so we install the project
        # itself with .[X,Y] after the requirements file. Falls
        # through cleanly when EXTRAS is empty (no install step).
        # all-extras on pip can't be honoured (it would require
        # parsing pyproject to enumerate them); warn so a pipeline
        # that targets multiple managers gets a clear message
        # instead of silent divergence.
        if [ "${ALL_EXTRAS}" = "true" ]; then
            echo "gocdnext/python: all-extras: true has no pip equivalent — enumerate extras: [a, b, ...] instead" >&2
        elif [ "${#EXTRAS[@]}" -gt 0 ]; then
            if [ -f "pyproject.toml" ] || [ -f "setup.py" ] || [ -f "setup.cfg" ]; then
                # Comma-join (no spaces) — pip's bracket syntax requires it.
                IFS=, eval 'extras_csv="${EXTRAS[*]}"'
                pip install -e ".[${extras_csv}]"
            else
                echo "gocdnext/python: extras requested but no pyproject.toml/setup.py — extras only attach to a package spec on pip" >&2
                exit 2
            fi
        fi
        rewrite_venv_shebangs
        exec bash -lc -- "${PLUGIN_COMMAND}"
        ;;
    none)
        echo "==> skipping dependency install (manager=none)"
        exec bash -lc -- "${PLUGIN_COMMAND}"
        ;;
    *)
        echo "gocdnext/python: unknown manager '${MANAGER}' (accepted: auto, uv, pip, poetry, none)" >&2
        exit 2
        ;;
esac
