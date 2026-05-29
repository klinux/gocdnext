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

cd "${WORKING_DIR}"

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
case "${MANAGER}" in
    poetry)
        poetry config virtualenvs.in-project true
        poetry install --no-interaction
        # Activate the in-project venv directly instead of going
        # through `poetry run` / `uv run`. v0.4.23 tried `poetry
        # run -- bash -lc "..."` but uv 0.5.5 still mangled the
        # `-lc` argv into a bare `-`, breaking with `bash: - :
        # invalid option`. Sourcing the venv's activate gives the
        # same PATH + VIRTUAL_ENV the wrapper would set, and the
        # subsequent `bash -lc` is invoked directly by this script
        # so nothing else can touch the args. Same pattern the pip
        # branch has always used.
        # shellcheck disable=SC1091
        source .venv/bin/activate
        exec bash -lc "${PLUGIN_COMMAND}"
        ;;
    uv)
        if [ -f "uv.lock" ]; then
            uv sync --frozen
        else
            uv sync
        fi
        # shellcheck disable=SC1091
        source .venv/bin/activate
        exec bash -lc "${PLUGIN_COMMAND}"
        ;;
    pip)
        req="${PLUGIN_REQUIREMENTS:-requirements.txt}"
        if [ ! -f "${req}" ]; then
            echo "gocdnext/python: pip manager selected but ${req} not found" >&2
            exit 2
        fi
        python -m venv .venv
        # shellcheck disable=SC1091
        source .venv/bin/activate
        # pip's cache dir default was redirected to
        # .cache/pip at the top of this script so a
        # pipeline `cache:` block can preserve the wheel cache
        # across runs — skip --no-cache-dir here.
        pip install -r "${req}"
        exec bash -lc "${PLUGIN_COMMAND}"
        ;;
    none)
        echo "==> skipping dependency install (manager=none)"
        exec bash -lc "${PLUGIN_COMMAND}"
        ;;
    *)
        echo "gocdnext/python: unknown manager '${MANAGER}' (accepted: auto, uv, pip, poetry, none)" >&2
        exit 2
        ;;
esac
