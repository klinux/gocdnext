# `gocdnext/node` v4

Node.js install + run runner. Selects the **node major** from the
project's own declaration (`engines.node` / `.nvmrc` / `.node-version`)
across the baked 22/24/26, auto-detects the manager (**pnpm / npm /
yarn v3+ / bun**) from the lockfile, sets up corepack (or the baked bun)
+ a workspace-local store, runs the dependency install (skippable), then
executes your shell command.

> **Breaking change from v3 (#373)**: the baked matrix drops **EOL node 20**
> — it is now **22/24/26**. Node 20 reached end-of-life on 2026-04-30, so v4
> no longer ships it. A pipeline resolving to node 20 now **fails loud**
> (exit 2) with a migration message instead of running a different node. See
> [migration from v3](#migration-from-v3-node-20-dropped). `@v3` stays
> available **frozen** (still baking 20/22/24) for a temporary bridge — it is
> not rebuilt or patched.
>
> **v3 vs v2**: adds **bun** + **node-version selection**; base is
> debian-slim (glibc) with `build-essential` + `python3` so node-gyp native
> builds work. Node/bun are **baked** — selection is a PATH switch with no
> runtime download.
>
> **Breaking change from v1**: see [migration](#migration-from-v1) below.

## Usage

```yaml
jobs:
  deps:
    stage: install
    uses: gocdnext/node@v4
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
    uses: gocdnext/node@v4
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
    uses: gocdnext/node@v4
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
| `node-version` | `PLUGIN_NODE_VERSION` | `auto`  | `22` / `24` / `26` / `auto`. `auto` reads `engines.node`, else `.node-version` / `.nvmrc`; defaults to 22. Explicit value overrides the declaration (printed). An unbaked major fails loud — no silent fallback. Node 20 (EOL) was dropped in v4 → pins to it fail loud. PATH switch, no download. |
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
  image (only **22 / 24 / 26**) fails loud with the baked set — never a
  silent fallback to the default node. Node 20 (EOL 2026-04-30) was
  dropped in v4; a pipeline pinned to it fails loud with a migration hint.

## Node version

The image bakes node **22**, **24**, and **26**; the entrypoint selects
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

- `">=18"` → node 22 (smallest baked that satisfies; not rejected just
  because 18 isn't baked);
- `"18 - 24"`, `">= 18"` (spaces / hyphen ranges) → node 22;
- `">=22.99.0"` → node 24 (baked 22 is 22.23.x which does **not** satisfy →
  skipped; a major-only parser would wrongly run 22 here);
- `"<24"` → node 22 (upper bound honored; 24/26 excluded);
- `"20.x"`, `"<22"`, `"^18"`, `">=26.99.0"`, `">26"` → **fail loud** (no baked
  version satisfies) — never a version the range excludes. **`20.x`/`<22`
  are the dropped-node-20 cases: migrate the declaration to 22|24|26;**
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

## Migration from v3 (node 20 dropped)

v4 drops **node 20** from the baked matrix — it reached end-of-life on
2026-04-30 and no longer receives upstream security or bug fixes, so the
maintained image stops shipping it. The matrix is now **22 / 24 / 26**.

What happens to a pipeline pinned to node 20 on `@v4`: version selection
**fails loud** (exit 2) — `engines.node: "20.x"`, `node-version: 20`,
`.nvmrc`/`.node-version` = `20`, or a `<22` range all stop the job with the
baked set + a "migrate to 22|24|26" message. The job never runs on a
different node silently.

To migrate, move your declaration to a supported major:

| Pinned to node 20 (v3)            | Fix (v4)                                  |
|-----------------------------------|-------------------------------------------|
| `engines.node: "20.x"`            | `engines.node: "22.x"` (or `24`/`26`)     |
| `node-version: 20`                | `node-version: 22` (or `24`/`26`)         |
| `.nvmrc` / `.node-version` = `20` | `22` (or `24`/`26`)                       |
| needs an exact node 20 build      | run the job on a custom `image:`          |

**Temporary bridge:** `@v3` stays in the registry **frozen** at its last
digest (still baking 20/22/24). It is **not rebuilt or security-patched**,
so treat it as a short-lived bridge while migrating — or pin a gocdnext
release tag to hold the whole catalog. Prefer migrating off node 20.

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
