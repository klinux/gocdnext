# `gocdnext/node`

A pnpm-first Node.js plugin for gocdnext pipelines. Wraps the
corepack + pnpm dance so every Node job doesn't re-paste the same
`corepack enable && corepack prepare --activate && pnpm …` boilerplate.

## Usage

```yaml
jobs:
  deps:
    stage: install
    uses: gocdnext/node
    with:
      working-dir: web
      command: install --frozen-lockfile
    artifacts:
      paths: [web/node_modules/]

  typecheck:
    stage: lint
    uses: gocdnext/node
    needs: [deps]
    needs_artifacts:
      - from_job: deps
        paths: [web/node_modules/]
    with:
      working-dir: web
      command: exec tsc --noEmit

  unit:
    stage: test
    uses: gocdnext/node
    needs: [deps]
    needs_artifacts:
      - from_job: deps
        paths: [web/node_modules/]
    with:
      working-dir: web
      command: test --run
```

## Settings (all become `PLUGIN_*` env)

| Setting       | Env var              | Required | Notes                                              |
|---------------|----------------------|----------|----------------------------------------------------|
| `command`     | `PLUGIN_COMMAND`     | yes      | Arguments to `pnpm`, word-split (e.g. `build`).    |
| `working-dir` | `PLUGIN_WORKING_DIR` | no       | Path under `/workspace`. Defaults to repo root.    |

Anything under the job's `variables:` lands as a regular env var (the
runner layers job env on top of plugin env), so for things like build
arguments:

```yaml
bundle:
  stage: build
  uses: gocdnext/node
  variables:
    GOCDNEXT_API_URL: http://localhost:8153
  with:
    working-dir: web
    command: build
```

## Building the image locally

The plugin image isn't pushed to a registry yet — for dev, build it
on the agent host so `docker run` finds it without a pull:

```
make plugins
# or
docker build -t gocdnext/node plugins/node/
```

## Contract recap

1. Image's entrypoint handles the logic — no user script needed.
2. Settings in the job YAML become `PLUGIN_*` env vars (kebab-case
   `working-dir` → `PLUGIN_WORKING_DIR`, camelCase `targetEnv` →
   `PLUGIN_TARGET_ENV`).
3. Exit 0 = success. Anything else fails the job.
4. Logs on stdout/stderr stream back to the UI via the agent's
   existing log pipe.
