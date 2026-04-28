# gocdnext/codecov — upload a coverage report to Codecov via the
# official `codecov` CLI (the Rust rewrite that replaced the old
# bash uploader in 2023). Uses a repo token for auth so uploads
# work from private forks too — the old auto-discover-token
# trick needed GitHub Actions metadata that a generic CI doesn't
# ship.
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_TOKEN        (required)  Codecov repo upload token.
#                                   Populate via secrets:.
#   PLUGIN_FILE         (optional)  Explicit coverage file
#                                   (cover.out, coverage.xml, …).
#                                   When omitted the CLI
#                                   auto-detects common file
#                                   names at the working-dir root.
#   PLUGIN_FLAGS        (optional)  comma-separated Codecov
#                                   flags — e.g. "unit,integration"
#                                   tags the upload so the Codecov
#                                   UI can split coverage by stage.
#   PLUGIN_WORKING_DIR  (optional)  cd here first. Default ".".
#   PLUGIN_SLUG         (optional)  "owner/repo" override.
#                                   Codecov normally pulls this
#                                   from git remote; set it when
#                                   the working-dir isn't a git
#                                   checkout (e.g. uploaded
#                                   artifact).
#   PLUGIN_URL          (optional)  Codecov Enterprise URL. The
#                                   cloud default is codecov.io.
# Codecov CLI v10 is a Python package (the old static binary at
# cli.codecov.io/<v>/linux/codecov is a PyInstaller bundle whose
# loader chokes on slim images of either libc family — alpine
# musl OR debian-slim glibc). Installing via pip from python:slim
# bypasses that path entirely and gives us a `codecov` entrypoint
# that runs natively for both amd64 and arm64.
FROM python:3.12-slim-bookworm

RUN apt-get update && apt-get install -y --no-install-recommends \
        bash ca-certificates git \
    && rm -rf /var/lib/apt/lists/*

ARG CODECOV_VERSION=10.3.0
# codecov-cli installs BOTH `codecov` and `codecovcli` console
# scripts, so the entrypoint that calls `codecov` keeps working
# without a manual symlink.
RUN pip install --no-cache-dir "codecov-cli==${CODECOV_VERSION}" \
    && codecov --version

COPY entrypoint.sh /usr/local/bin/gocdnext-codecov
RUN chmod +x /usr/local/bin/gocdnext-codecov

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-codecov"]
