---
title: Python (pip / Poetry / uv)
description: Python project recipes — install, lint, test, package — covering pip, Poetry, and the modern uv workflow.
---

The [`gocdnext/python`](/gocdnext/docs/reference/plugins/#python)
plugin auto-detects the manager (priority: poetry.lock → uv.lock →
requirements.txt → pyproject.toml), runs a frozen install, then
executes the user's shell `command:`. Per-manager cache
directories all land under `.cache/` so a single cache entry
covers all three.

## Layout assumed

```
repo/
├── pyproject.toml
├── uv.lock | poetry.lock | requirements.txt
├── src/
│   └── mypkg/...
└── tests/...
```

This recipe shows three variants of the same shape — pick the
one that matches your project. The plugin handles install
automatically based on lockfile detection; you write the shell
command that runs against the resolved environment.

## Recipe — uv (recommended for new projects)

[`uv`](https://github.com/astral-sh/uv) is the fastest Python
installer. With `uv.lock` present, the plugin auto-selects uv
and runs `uv sync --frozen` before `command:`.

```yaml title=".gocdnext/ci.yaml"
name: ci
when:
  event: [push, pull_request]
stages: [deps, lint, test]

jobs:
  install:
    stage: deps
    uses: gocdnext/python@v1
    with:
      command: "true"          # install-only — plugin handles uv sync --frozen
      all-extras: "true"       # bring in dev/test extras for downstream jobs
    cache:
      - key: uv-{{ hash "uv.lock" }}
        paths: [.cache, .venv]
    artifacts:
      paths: [.venv, uv.lock]

  ruff:
    stage: lint
    uses: gocdnext/python@v1
    needs: [install]
    needs_artifacts:
      - from_job: install
        paths: [.venv, uv.lock]
    with:
      no-install: "true"       # reuse artifact venv as-is
      command: ruff check src tests

  mypy:
    stage: lint
    uses: gocdnext/python@v1
    needs: [install]
    needs_artifacts:
      - from_job: install
        paths: [.venv, uv.lock]
    with:
      no-install: "true"
      command: mypy src

  pytest:
    stage: test
    uses: gocdnext/python@v1
    needs: [install]
    needs_artifacts:
      - from_job: install
        paths: [.venv, uv.lock]
    with:
      no-install: "true"
      command: pytest --junit-xml=junit.xml --cov=src --cov-report=xml
    test_reports:
      - junit.xml
    artifacts:
      paths: [coverage.xml]
```

Plugin auto-selects uv because `uv.lock` is present.
`all-extras: "true"` translates to `uv sync --all-extras` so the
single install resolves dev tooling once; downstream jobs use
`no-install: "true"` to skip re-resolve.

## Recipe — Poetry

```yaml
jobs:
  install:
    stage: deps
    uses: gocdnext/python@v1
    with:
      command: "true"
      extras: "dev, test"      # poetry: --extras "dev test"
    cache:
      - key: poetry-{{ hash "poetry.lock" }}
        paths: [.cache, .venv]
    artifacts:
      paths: [.venv]

  pytest:
    stage: test
    uses: gocdnext/python@v1
    needs: [install]
    needs_artifacts:
      - from_job: install
        paths: [.venv]
    with:
      no-install: "true"
      command: pytest --junit-xml=junit.xml
    test_reports:
      - junit.xml
```

Plugin sets `POETRY_VIRTUALENVS_IN_PROJECT=1` so `.venv/` lands
in the workspace where artefacts can pick it up.
`POETRY_CACHE_DIR=/workspace/.cache/pypoetry` redirects the
download cache too.

## Recipe — pip + requirements.txt

```yaml
jobs:
  install:
    stage: deps
    uses: gocdnext/python@v1
    with:
      manager: pip
      requirements: requirements-dev.txt   # combined dev + runtime list
      command: "true"
    cache:
      - key: pip-{{ hash "requirements-dev.txt" }}
        paths: [.cache]
    artifacts:
      paths: [.venv]

  pytest:
    stage: test
    uses: gocdnext/python@v1
    needs: [install]
    needs_artifacts:
      - from_job: install
        paths: [.venv]
    with:
      no-install: "true"
      command: pytest --junit-xml=junit.xml
    test_reports:
      - junit.xml
```

Plain pip on `requirements.txt` is the simplest setup but the
slowest to warm — there's no native lockfile, so the plugin pins
the requirements file via `--no-deps --no-build-isolation` and
relies on it being fully pinned upstream.

## Variations

### Coverage upload

```yaml
upload-coverage:
  stage: test
  uses: gocdnext/codecov@v1
  needs: [pytest]
  needs_artifacts:
    - from_job: pytest
      paths: [coverage.xml]
  with:
    file: coverage.xml
    flags: python
  secrets:
    - CODECOV_TOKEN
```

### Build a wheel and publish on tag

Per-job `when:` isn't enforced today. The clean separation is one
pipeline that always builds + uploads the wheel as an artefact,
and a **separate publish pipeline** triggered only on tag.

`.gocdnext/ci.yaml`:

```yaml
package:
  stage: build
  uses: gocdnext/python@v1
  needs: [install]
  needs_artifacts:
    - from_job: install
      paths: [.venv]
  with:
    no-install: "true"
    command: python -m build
  artifacts:
    paths: ["dist/*.whl", "dist/*.tar.gz"]
```

`.gocdnext/release.yaml`:

```yaml
name: release
when:
  event: [tag]
stages: [publish]
jobs:
  twine-upload:
    stage: publish
    uses: gocdnext/python@v1
    with:
      no-install: "true"
      command: |
        python -m twine upload --non-interactive dist/*
    secrets:
      - TWINE_USERNAME
      - TWINE_PASSWORD       # often "__token__" + a PyPI API token
```

The tag trigger on the release pipeline fires only on `vX.Y.Z`
push; the main `ci` pipeline still produces the wheel on every
push for parity with non-release runs.

### Multiple Python versions (matrix)

The python plugin's image is locked to one interpreter, so a
matrix across versions can't just vary a `with:` input —
matrix-testing across versions means bypassing the plugin and
calling pip/uv directly inside `python:3.X-slim`:

```yaml
pytest:
  stage: test
  image: python:${{ PY_VERSION }}-slim
  parallel:
    matrix:
      - PY_VERSION: ["3.11", "3.12", "3.13"]
  script:
    - pip install --root-user-action=ignore -e ".[test]"
    - pytest --junit-xml=junit.xml
  test_reports:
    - junit.xml
```

`parallel.matrix:` is the **list of objects** shape — each entry
maps a name to a list of values. `${{ PY_VERSION }}` substitution
expands per cell.

## Common pitfalls

- **System Python vs venv**: gocdnext's `python` plugin sets up
  the manager's expected venv. If you skip the install step and
  run plain `python -m pytest`, the system interpreter has no
  project dependencies. Always go through `pytest` (the venv
  shim) or `python -m pytest` from a job that has `no-install:
  "false"` (the default).
- **Pip cache vs wheel cache**: `--no-cache-dir` on pip disables
  the wheel cache (good for reproducibility) but the platform's
  `cache:` block still preserves the install state via
  `site-packages`. The two are independent.
- **Coverage XML path**: pytest-cov writes to `coverage.xml` by
  default but only when `--cov-report=xml` is set. Without it,
  the file doesn't exist and `optional:` artefacts silently
  no-op.
