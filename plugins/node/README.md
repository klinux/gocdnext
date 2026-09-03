# `gocdnext/node` v3

Node.js install + run runner. Selects the **node major** from the
project's own declaration (`engines.node` / `.nvmrc` / `.node-version`)
across the baked 20/22/24, auto-detects the manager (**pnpm / npm /
yarn v3+ / bun**) from the lockfile, sets up corepack (or the baked bun)
+ a workspace-local store, runs the dependency install (skippable), then
executes your shell command.

> **v3 vs v2**: adds **bun** + **node-version selection**; base moved
> alpine → debian-slim (glibc) with `build-essential` + `python3` so
> node-gyp native builds work. Node/bun are **baked** — selection is a
> PATH switch with no runtime download. `@v2` stays available for
> pipelines pinned to it.
>
> **Breaking change from v1**: see [migration](#migration-from-v1) below.

## Usage

```yaml
jobs:
  deps:
    stage: install
    uses: gocdnext/node@v3
    with:
      working-dir: web
      # install: true (default) + command: "" (default) →
      # install-only job. Plugin runs `pnpm install --frozen-lockfile`
      # automatically.
    cache:
      - key: pnpm-store-{{ hash "web/pnpm-lock.yaml" }}
        paths: [web/.pnpm-store]
    artifacts:
      paths: [web/node_modules]

  typecheck:
    stage: lint
    uses: gocdnext/node@v3
    needs: [deps]
    needs_artifacts:
      - from_job: deps
        paths: [web/node_modules]
    with:
      working-dir: web
      install: false              # reuse the artifact, skip resolve
      command: pnpm exec tsc --noEmit

  unit:
    stage: test
    uses: gocdnext/node@v3
    needs: [deps]
    needs_artifacts:
      - from_job: deps
        paths: [web/node_modules]
    with:
      working-dir: web
      install: false
      command: pnpm test --run
```

## Inputs (all become `PLUGIN_*` env)

| Setting       | Env var              | Default | Notes                                              |
|---------------|----------------------|---------|----------------------------------------------------|
| `command`      | `PLUGIN_COMMAND`      | `""`    | Shell command via `bash -lc` — `&&` / pipes / env expansion all work. NOT prefixed with a manager. |
| `working-dir`  | `PLUGIN_WORKING_DIR`  | `.`     | Path under `/workspace`. |
| `node-version` | `PLUGIN_NODE_VERSION` | `auto`  | `20` / `22` / `24` / `auto`. `auto` reads `engines.node`, else `.node-version` / `.nvmrc`; defaults to 22. Explicit value overrides the declaration (printed). An unbaked major fails loud — no silent fallback. PATH switch, no download. |
| `manager`      | `PLUGIN_MANAGER`      | `auto`  | `pnpm` / `npm` / `yarn` (v3+) / `bun` / `none` / `auto`. Auto detects via lockfile (priority `bun.lock`/`bun.lockb` > `pnpm-lock.yaml` > `yarn.lock` + `.yarnrc.yml` > `package-lock.json`; a `packageManager: "bun@x"` with no lockfile also picks bun). `none` skips install entirely. |
| `install`      | `PLUGIN_INSTALL`      | `true`  | Run the manager's frozen install before `command`. Set `false` to reuse a `node_modules/` artifact. |
| `frozen`       | `PLUGIN_FROZEN`       | `true`  | pnpm → `--frozen-lockfile`; npm → `npm ci` vs `npm install`; yarn v3+ → `--immutable`; bun → `--frozen-lockfile`. |
| `prod`         | `PLUGIN_PROD`         | `false` | Skip dev deps. pnpm → `--prod`; npm → `--omit=dev`; bun → `--production`; yarn v3+ → `yarn workspaces focus --all --production` (requires the `workspace-tools` plugin: bundled in Yarn 4, opt-in via `yarn plugin import workspace-tools` in Yarn 3). |

Boolean inputs accept `true`/`false`/`1`/`0`/`yes`/`no` (case-insensitive).
Typos like `flase` are rejected at plugin start — silent intent-flips
were the v1 footgun.

`manager` is enum-validated post-resolution: any value outside the
closed set (after lowercase + auto-resolve) fails fast with an error
listing the accepted set.

## Validation rules

- `command: ""` requires `install: true` AND `manager != none` —
  otherwise the job would do nothing and exit 0 (silent no-op).
- `manager: yarn` against a repo with `yarn.lock` but no `.yarnrc.yml`
  is rejected as yarn v1 (maintenance-only upstream). Use `pnpm`/`npm`,
  upgrade to yarn v3+, or fall back to `manager: none` + a script.
- Auto-detect with no lockfile fails with a clear "add a lockfile or
  set manager explicitly" message rather than guessing.
- A `node-version` (or `engines.node`) major that isn't baked into the
  image (only **20 / 22 / 24**) fails loud with the baked set — never a
  silent fallback to the default node.

## Node version

The image bakes node **20**, **22**, and **24**; the entrypoint selects
one per run and points `PATH` at it — a switch, not a download. Source
of truth, in order:

1. `node-version:` input (explicit override).
2. `engines.node` in `package.json` — e.g. `"24.x"`, `"^24"`, `">=24"`.
3. `.node-version` / `.nvmrc`.
4. Default **22**.

`engines.node` is evaluated with the **real `semver`** bundled in the
image's npm (no download), against the baked versions — so minor/patch
bounds, operator spaces and hyphen ranges are honored exactly, not
approximated. The plugin picks the **smallest baked node version that
satisfies the range**:

- `">=18"` → node 20 (20 satisfies; not rejected just because 18 isn't baked);
- `"18 - 22"`, `">= 18"` (spaces / hyphen ranges) → node 20;
- `">=20.99.0"` → node 22 (baked 20 is 20.20.2 which does **not** satisfy →
  skipped; the old major-only logic wrongly ran 20 here);
- `">=18 <20"`, `"^18"`, `"18.x"`, `"<20"`, `">24"`, `">=24.99.0"` → **fail
  loud** (no baked version satisfies) — never a version the range excludes;
- an invalid range → fail loud;
- a value with **no numeric major** (an `lts/*` alias, `*`) falls back to
  the default — not a hard error.

`.node-version` / `.nvmrc` and an explicit `node-version:` are treated as
**exact** majors: an unbaked one fails loud. An explicit `node-version:`
that disagrees with `engines.node` wins and is printed so the divergence
is visible. Need a version that isn't baked? Pin a supported major, or run
the job on a custom `image:` (bake your own CI image from your app's
Dockerfile base stage).

## Cache paths per manager

The plugin redirects each manager's store/cache to a workspace-relative
path so the platform's `cache:` block can tar it:

| Manager  | Cache path        |
|----------|-------------------|
| pnpm     | `.pnpm-store/`    |
| npm      | `.npm-cache/`     |
| yarn v3+ | `.yarn/cache/`    |
| bun      | `.bun-cache/`     |

Combine with `cache: { key: <manager>-{{ hash "<lockfile>" }} }` for
deterministic invalidation on lockfile change (gocdnext ≥ v0.4.37).

## Migration from v1

v1 was a thin `pnpm <command>` prefixer. v2 separates install from
command and shell-evaluates `command:`. The `:v1` rolling tag now
points at the v2 image — pin to `:0.4.38` to stay on the old
contract while migrating.

| v1 YAML                                 | v2 YAML                                                                 |
|-----------------------------------------|-------------------------------------------------------------------------|
| `command: install --frozen-lockfile`    | (drop — defaults `install:true frozen:true` do this)                    |
| `command: --filter @web lint`           | `command: pnpm --filter @web lint`                                      |
| `command: exec tsc --noEmit`            | `install: false` + `command: pnpm exec tsc --noEmit` (artifact-restore) |
| `command: test --run`                   | `command: pnpm test --run`                                              |
| `command: build`                        | `command: pnpm build`                                                   |

## Building the image locally

```
make plugins
# or
docker build -t gocdnext/node:dev plugins/node/
```

## Contract recap

1. Image's entrypoint handles the logic — no user script needed.
2. Settings in the job YAML become `PLUGIN_*` env vars (kebab-case
   `working-dir` → `PLUGIN_WORKING_DIR`).
3. Exit 0 = success. Anything else fails the job.
4. Stdout/stderr stream back to the UI via the agent's existing log pipe.
