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
# /mypy, /alembic, …) carry a shebang like
#   `#!/workspace/<install-job-uuid>/.../.venv/bin/python`
# which is exactly what pip/uv wrote at install time — pointing at
# the upstream job's workspace. When the consumer job's kernel
# tries to exec the script, that interpreter path is gone and
# exec() returns ENOENT — surfaces as the opaque
#   `Failed to spawn: ruff / No such file or directory`.
# `uv venv --relocatable` (tried in v0.4.29) only makes the
# activate script portable, NOT the entry-point shebangs.
#
# Fix: rewrite the first line of each .venv/bin/* script to point
# at THIS job's `$PWD/.venv/bin/python` so the entry-points spawn
# correctly. Idempotent — if the shebang already matches our path,
# sed substitutes a line with the same content. The venv's own
# `python` lives as a SYMLINK (not a script) so we only touch
# regular files. Fast: a typical venv has ~30 entry-point scripts.
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
    local venv_python="$(pwd)/.venv/bin/python"
    [ -e "$venv_python" ] || return 0
    local f
    for f in .venv/bin/*; do
        [ -f "$f" ] || continue
        # Only touch scripts that start with a python shebang. The
        # `IFS= read` form gives us the first line without slurping
        # binaries; on a non-text file the read fails and we skip.
        IFS= read -r line < "$f" 2>/dev/null || continue
        case "$line" in
            "#!"*python*)
                sed -i "1c\\#!$venv_python" "$f"
                ;;
        esac
    done
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
