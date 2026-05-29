#!/bin/bash
# gocdnext/rust — thin wrapper around `cargo`. See Dockerfile for
# the full contract.

set -euo pipefail

if [ -z "${PLUGIN_COMMAND:-}" ]; then
    echo "gocdnext/rust: PLUGIN_COMMAND is required" >&2
    echo "  example: command: test --workspace" >&2
    exit 2
fi

WORKING_DIR="${PLUGIN_WORKING_DIR:-.}"
TOOLCHAIN="${PLUGIN_TOOLCHAIN:-stable}"

cd "${WORKING_DIR}"

# Point $CARGO_HOME at a PWD-relative dir by default so the
# platform's `cache:` block can tar the registry index + crate
# archives + git DB. Set AFTER cd so caches sit next to the
# project, not next to the monorepo root when WORKING_DIR is a
# sub-directory. Without this override they'd land in
# /usr/local/cargo (the base image's CARGO_HOME) which the agent
# doesn't tar. RUSTUP_HOME is left untouched — it holds the
# toolchain binaries; caching those adds GB for no reuse gain.
export CARGO_HOME="${CARGO_HOME:-.cargo-home}"
mkdir -p "${CARGO_HOME}"

# Install the requested toolchain if it's not already present.
# `rustup toolchain install` is a no-op when the toolchain is
# already there, so this is cheap on re-runs.
if [ "${TOOLCHAIN}" != "stable" ]; then
    echo "==> ensuring toolchain ${TOOLCHAIN}"
    rustup toolchain install "${TOOLCHAIN}" --profile minimal --component rustfmt --component clippy
    rustup default "${TOOLCHAIN}"
fi

if [ -n "${PLUGIN_RUSTFLAGS:-}" ]; then
    export RUSTFLAGS="${PLUGIN_RUSTFLAGS}"
    echo "==> RUSTFLAGS=${RUSTFLAGS}"
fi

# shellcheck disable=SC2086
# Intentional word-splitting: users write "test --workspace" and
# expect cargo to see two args. Quoting would break that.
echo "==> cargo ${PLUGIN_COMMAND}"
exec cargo ${PLUGIN_COMMAND}
