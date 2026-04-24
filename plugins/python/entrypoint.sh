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

cd "/workspace/${WORKING_DIR}"

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
        # Prefix the command so pytest/ruff/etc resolve from the
        # poetry-managed venv without the user having to write
        # `poetry run` in every plugin example.
        exec poetry run bash -lc "${PLUGIN_COMMAND}"
        ;;
    uv)
        if [ -f "uv.lock" ]; then
            uv sync --frozen
        else
            uv sync
        fi
        exec uv run bash -lc "${PLUGIN_COMMAND}"
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
        pip install --no-cache-dir -r "${req}"
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
