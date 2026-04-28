---
title: Python (pip / Poetry / uv)
description: Python project recipes — install, lint, test, package — covering pip, Poetry, and the modern uv workflow.
---

The [`gocdnext/python`](/gocdnext/docs/reference/plugins/#python)
plugin ships an image with the interpreter + the standard packaging
tools. The plugin works the same way regardless of which dependency
manager you use; the difference is in `command:` and which cache
paths the platform preserves.

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
one that matches your project.

## Recipe — uv (recommended for new projects)

[`uv`](https://github.com/astral-sh/uv) is the fastest Python
installer (Rust, parallel resolution, drop-in replacement for
pip + pip-tools + venv). The dependency cache survives in
`/workspace/.uv-cache`.

```yaml title=".gocdnext/ci.yaml"
name: ci
when:
  event: [push, pull_request]
stages: [lint, test]

jobs:
  install:
    stage: lint
    uses: gocdnext/python@v1
    with:
      command: -m uv sync --frozen
    cache:
      - key: uv-${CI_COMMIT_BRANCH}
        paths: [.uv-cache, .venv]
    artifacts:
      paths: [".venv/"]

  ruff:
    stage: lint
    uses: gocdnext/python@v1
    needs: [install]
    needs_artifacts:
      - from_job: install
        paths: [".venv/"]
    with:
      command: -m uv run ruff check src tests

  mypy:
    stage: lint
    uses: gocdnext/python@v1
    needs: [install]
    needs_artifacts:
      - from_job: install
        paths: [".venv/"]
    with:
      command: -m uv run mypy src

  pytest:
    stage: test
    uses: gocdnext/python@v1
    needs: [install]
    needs_artifacts:
      - from_job: install
        paths: [".venv/"]
    with:
      command: -m uv run pytest --junit-xml=junit.xml --cov=src --cov-report=xml
    test_reports:
      paths: [junit.xml]
    artifacts:
      paths: [coverage.xml]
```

`uv sync --frozen` reads `uv.lock` and refuses to update — same
posture as `pnpm install --frozen-lockfile`. A drifted lockfile
fails the install, which is what you want in CI.

## Recipe — Poetry

```yaml
install:
  stage: lint
  uses: gocdnext/python@v1
  with:
    command: -m poetry install --no-interaction --no-ansi
  cache:
    - key: poetry-${CI_COMMIT_BRANCH}
      paths: [.cache/pypoetry, .venv]
  artifacts:
    paths: [".venv/"]

pytest:
  stage: test
  uses: gocdnext/python@v1
  needs: [install]
  needs_artifacts:
    - from_job: install
      paths: [".venv/"]
  with:
    command: -m poetry run pytest --junit-xml=junit.xml
  test_reports:
    paths: [junit.xml]
```

`POETRY_VIRTUALENVS_IN_PROJECT=1` (set by the plugin) puts
`.venv/` in the workspace so it travels via the artefacts pass.
`POETRY_CACHE_DIR=/workspace/.cache/pypoetry` ditto for the
package archive cache.

## Recipe — pip + requirements.txt

```yaml
install:
  stage: lint
  uses: gocdnext/python@v1
  with:
    command: -m pip install --no-cache-dir -r requirements.txt -r requirements-dev.txt
  cache:
    - key: pip-${CI_COMMIT_BRANCH}
      paths: [.pip-cache]
  artifacts:
    paths: [".local/lib/python*/site-packages/"]
```

Plain pip on requirements.txt is the simplest setup but the slowest
to warm — there's no lockfile equivalent, so resolution runs every
install. Pin everything (no `>=` in `requirements.txt`) to avoid
surprise upgrades on a CI machine that happens to resolve
differently.

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

### Build a wheel and publish

```yaml
package:
  stage: build
  uses: gocdnext/python@v1
  with:
    command: -m build
  artifacts:
    paths: ["dist/*.whl", "dist/*.tar.gz"]

publish:
  stage: build
  uses: gocdnext/python@v1
  needs: [package]
  needs_artifacts:
    - from_job: package
      paths: ["dist/"]
  when:
    event: [tag]
  with:
    command: -m twine upload --non-interactive dist/*
  secrets:
    - TWINE_USERNAME
    - TWINE_PASSWORD     # often the literal "__token__" + a PyPI API token
```

The `when.event: [tag]` gate means publish only fires on push of
a `vX.Y.Z` tag — push to a branch creates the wheel artefact but
doesn't ship it.

### Multiple Python versions (matrix)

```yaml
pytest:
  stage: test
  uses: gocdnext/python@v1
  needs: [install]
  matrix:
    python_version: ["3.10", "3.11", "3.12", "3.13"]
  with:
    image: python:${{ matrix.python_version }}-slim
    command: -m pytest
```

The matrix expands to 4 parallel jobs; each runs in its own
container with the appropriate interpreter. Cache keys can
include `${{ matrix.python_version }}` so each cell warms its
own bucket.

## Common pitfalls

- **System Python vs venv**: gocdnext's `python` plugin defaults
  to running inside `.venv/` when one exists — same posture as
  every package manager. If you skip the install step and try
  `python -m pytest`, the system interpreter has no project
  dependencies. Always run through `uv run` / `poetry run` or
  the venv's `python`.
- **Pip cache vs wheel cache**: `--no-cache-dir` on pip disables
  the WHEEL cache (good for reproducibility) but the platform's
  `cache:` block still preserves the install state via
  `site-packages`. The two are independent.
- **Coverage XML path**: pytest-cov writes to `coverage.xml` by
  default but only when `--cov-report=xml` is set. Without it,
  the file doesn't exist and `optional:` artefacts fire silent.
