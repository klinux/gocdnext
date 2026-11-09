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
#
# uv 0.5.5 → 0.13.7: 0.5.5 carried GHSA-hq85-5cpj-mr62 (CRITICAL) — its bundled
# CA store trusted an emSign root whose intermediate CA keys are compromised;
# fixed in 0.13.4. The plugin only uses uv's stable surface (`uv sync`,
# `--frozen`, `--extra`/`--all-extras`, `uv run`), which is unchanged across the
# jump; the smoke exercises a real `uv sync` + `uv run` to prove it.
RUN pip install --no-cache-dir --upgrade pip==24.3.1 \
    && pip install --no-cache-dir uv==0.13.7 poetry==1.8.4

COPY entrypoint.sh /usr/local/bin/gocdnext-python
RUN chmod +x /usr/local/bin/gocdnext-python

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-python"]
