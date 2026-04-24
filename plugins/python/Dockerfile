# gocdnext/python — Python build / test runner.
#
# Ships with:
#   - Python 3.12 (matches the `python:3.12` baseline the vast
#     majority of services pin to in 2026).
#   - `uv` — the fast installer/resolver from Astral. Covers
#     pyproject.toml + requirements.txt in one binary.
#   - `poetry` — still the most common pyproject manager for
#     libraries; the plugin auto-detects poetry.lock and uses it
#     when present.
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_COMMAND     (required)  shell command to run AFTER
#                                  dependency install. Typical:
#                                  "pytest", "pytest -n auto",
#                                  "ruff check .", "mypy src".
#   PLUGIN_WORKING_DIR (optional)  cd here first. Default ".".
#   PLUGIN_MANAGER     (optional)  "auto" (default) | "uv" | "pip"
#                                  | "poetry" | "none". "auto"
#                                  picks the first that matches a
#                                  file on disk (poetry.lock →
#                                  poetry; uv.lock →
#                                  uv; requirements.txt → pip;
#                                  pyproject.toml → uv).
#                                  "none" skips install entirely.
#   PLUGIN_REQUIREMENTS (optional) override path to a requirements
#                                  file (pip manager only).
FROM python:3.12-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends bash git curl ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Pinned versions — pip upgrades, poetry/uv stable. Keep these
# explicit so a silent upstream bump doesn't surprise CI.
RUN pip install --no-cache-dir --upgrade pip==24.3.1 \
    && pip install --no-cache-dir uv==0.5.5 poetry==1.8.4

COPY entrypoint.sh /usr/local/bin/gocdnext-python
RUN chmod +x /usr/local/bin/gocdnext-python

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-python"]
