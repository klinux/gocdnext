---
title: Node frontend (Next.js, Vite, etc.)
description: A pnpm-first Node.js project — type-check, test, build, with the pnpm content-addressable store cached across runs.
---

The [`gocdnext/node`](/gocdnext/docs/reference/plugins/#node) plugin
ships a corepack + pnpm shim that resolves the package manager from
`packageManager:` in `package.json`. Tests run, type-check is its
own gate, the production bundle is assembled, and the pnpm store
survives across runs so a warm install drops to seconds.

## Layout assumed

```
repo/
├── package.json       # with `packageManager: "pnpm@9.x.x"`
├── pnpm-lock.yaml
├── tsconfig.json
└── src/...
```

This recipe is what powers gocdnext's own dashboard build at
[`.gocdnext/ci-web.yaml`](https://github.com/klinux/gocdnext/blob/main/.gocdnext/ci-web.yaml).
Same shape works for Vite, Remix, plain TypeScript libraries.

## The pipeline

```yaml title=".gocdnext/ci-web.yaml"
name: ci-web

when:
  event: [push, pull_request]
  paths: ["web/**", "package.json", "pnpm-lock.yaml"]

stages: [install, lint, test, build]

jobs:
  deps:
    stage: install
    uses: gocdnext/node@v2
    with:
      working-dir: web
      # install: true (default) + command: "" (default) →
      # install-only job. Plugin runs `pnpm install --frozen-lockfile`
      # automatically.
    cache:
      - key: pnpm-store-{{ hash "web/pnpm-lock.yaml" }}
        paths: [web/.pnpm-store]
    artifacts:
      paths: ["web/node_modules/"]

  typecheck:
    stage: lint
    uses: gocdnext/node@v2
    needs: [deps]
    needs_artifacts:
      - from_job: deps
        paths: ["web/node_modules/"]
    with:
      working-dir: web
      install: false         # reuse the artifact, skip resolve
      command: pnpm exec tsc --noEmit

  unit:
    stage: test
    uses: gocdnext/node@v2
    needs: [deps]
    needs_artifacts:
      - from_job: deps
        paths: ["web/node_modules/"]
    with:
      working-dir: web
      install: false
      command: pnpm test --run
    test_reports:
      paths: ["web/junit.xml"]

  bundle:
    stage: build
    uses: gocdnext/node@v2
    needs: [typecheck, unit]
    needs_artifacts:
      - from_job: deps
        paths: ["web/node_modules/"]
    variables:
      # Build-time placeholder — real value lives at runtime.
      NEXT_PUBLIC_API_URL: http://localhost:8153
    with:
      working-dir: web
      install: false
      command: pnpm build
    artifacts:
      paths:
        - "web/.next/standalone/"
        - "web/.next/static/"
```

What's worth highlighting:

### `needs_artifacts:` is what passes `node_modules/` between jobs

Each job runs in a fresh container — the `deps` job's working tree
disappears at the end. `needs:` only orders jobs (it doesn't pass
files); `needs_artifacts:` pulls a tar of the listed paths from the
upstream job's artefact backend back into the downstream job's
workspace.

This pattern (install once, reuse) cuts a 4-job pipeline from
"install × 4" to "install × 1 + restore × 3". On a typical Next.js
project that's ~30 seconds saved per warm run.

### `install: true` (default) + `frozen: true` (default)

The v2 plugin handles dependency install automatically. With both
defaults on, the deps job's only `with:` line is `working-dir:` —
the plugin runs `pnpm install --frozen-lockfile` itself. Without
`frozen: true`, pnpm would UPDATE the lockfile if it disagreed with
the manifest; CI should never silently rewrite the lockfile, so
the flag turns a drift into a failed install. Set `frozen: false`
explicitly only when you intentionally want lockfile auto-fix
(rare in CI).

### `install: false` on downstream jobs

Lint / test / build jobs restore `node_modules/` via
`needs_artifacts:` from the upstream install. They DON'T re-run
the install — `install: false` skips it. Without that flag the
plugin would run a fresh `pnpm install --frozen-lockfile`,
re-resolving the lockfile and undoing the artifact restore.

### `pnpm-store` cache lives in the workspace

The plugin's entrypoint runs `pnpm config set store-dir
/workspace/.pnpm-store` so the platform's `cache:` block can tar
the content-addressable store. Default is `~/.local/share/pnpm/`
which the agent can't see.

`packageManager:` in `package.json` pins the pnpm version — the
plugin's corepack shim resolves it at runtime so two projects
with different pnpm versions can run on the same agent without
conflict.

### `pnpm exec tsc --noEmit`

`tsc --noEmit` is the type-only check. Running it as a separate
job from `unit` lets the run-detail UI show a clear "type errors"
vs "test failures" split — easier to triage than a single
combined job that fails for "some reason".

### `paths:` filter on `when:`

The `paths:` pattern under `when:` skips the run when the push
didn't touch the web directory. Without it, every backend-only PR
would still spin up the web pipeline for nothing. Patterns are
include-style globs.

## Variations

### Vite + Vitest with coverage

```yaml
unit:
  stage: test
  uses: gocdnext/node@v2
  needs: [deps]
  needs_artifacts:
    - from_job: deps
      paths: ["node_modules/"]
  with:
    install: false
    command: pnpm exec vitest run --coverage
  test_reports:
    paths: ["junit.xml"]
  artifacts:
    paths: ["coverage/"]

upload-coverage:
  stage: test
  uses: gocdnext/codecov@v1
  needs: [unit]
  needs_artifacts:
    - from_job: unit
      paths: ["coverage/lcov.info"]
  with:
    file: coverage/lcov.info
    flags: vite
  secrets:
    - CODECOV_TOKEN
```

### Lighthouse CI for performance budgets

```yaml
lighthouse:
  stage: test
  uses: gocdnext/lighthouse-ci@v1
  needs: [bundle]
  needs_artifacts:
    - from_job: bundle
      paths: ["dist/"]
  with:
    urls: |
      http://localhost:3000/
      http://localhost:3000/pricing
    number_of_runs: 3
  services:
    - name: app
      image: nginx:alpine
      # serve dist/ on port 3000 inside the agent network
```

### Playwright e2e

```yaml
e2e:
  stage: test
  uses: gocdnext/playwright@v1
  needs: [bundle]
  needs_artifacts:
    - from_job: bundle
      paths: ["dist/"]
  with:
    command: test --reporter=junit
  test_reports:
    paths: ["playwright-junit.xml"]
  artifacts:
    paths: ["playwright-report/"]
```

The Playwright plugin image ships Chromium, Firefox, and WebKit —
tests of all three browsers in one job.

### Monorepo — only the affected package

```yaml
when:
  event: [push, pull_request]
  paths: ["packages/web-app/**"]
```

For a Turborepo/Nx setup, `paths:` per-pipeline keeps backend-only
PRs from spinning up the web pipeline. Pair this with one
pipeline file per package so failures are scoped.

## Common pitfalls

- **`packageManager:` mismatch with the pnpm in CI**: corepack
  resolves at runtime, so the version in `package.json` is what
  runs. Update it in the same PR that bumps the lockfile or
  expect drift.
- **`pnpm-store` cache size**: real apps land at 1-2 GB. Bump
  `caches.projectQuotaBytes` in Helm if you're on the default
  100 GiB cluster cap with many projects.
- **`exec tsc` vs `run typecheck`**: prefer `exec tsc --noEmit`
  unless `package.json` has a custom typecheck script — `exec`
  bypasses the script lookup and goes straight to the binary.
- **`NODE_ENV=production` during `pnpm install`** prunes
  devDependencies — fine for runtime but breaks `pnpm test`
  later. Leave `NODE_ENV` alone in install, set it on the
  build/bundle job only.
