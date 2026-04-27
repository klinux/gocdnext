# gocdnext

> Modern CI/CD orchestrator. Cherry-picks the good ideas from **GoCD** (VSM,
> fanout, pipeline dependencies, stage/job model), **Woodpecker** (plugin =
> container), and **GitLab CI** (stages, rules, needs, matrix, extends).
> Written in Go. UI in Next.js. Container-native. Webhook-first.

Status: **alpha / internal use**. Not open to public yet.

[![Open in GitHub Codespaces](https://github.com/codespaces/badge.svg)](https://codespaces.new/klinux/gocdnext)
[![Open in Gitpod](https://img.shields.io/badge/Gitpod-ready--to--code-908a85?logo=gitpod)](https://gitpod.io/#https://github.com/klinux/gocdnext)

## Why another CI tool?

We loved GoCD's model (explicit stage → job → task, dependency materials, VSM)
but hated the stack: Java/Spring/Hibernate, XML config, poll-first, Rails UI,
no plugin marketplace. This is what GoCD would look like if we started today.

Differentiators vs. GitHub Actions / Tekton / Woodpecker:

- **Upstream material** — `pipeline B` waits for `pipeline A.stage X` to pass
  *with the same commit SHA*, with automatic fanout across N downstreams.
- **Value Stream Map (VSM)** — visualize the graph of pipelines + materials.
- **Webhook-first**, polling only as fallback.
- **Auto-register webhook on GitHub / GitLab / Bitbucket** when you create a
  git material.

## Repo layout

```
server/      Go control plane: HTTP API, gRPC for agents, scheduler, webhooks
agent/       Go agent: pulls jobs, runs containers, streams logs back
cli/         gocdnext CLI: validate, run-local
web/         Next.js 15 UI
proto/       gRPC / protobuf contracts
plugins/     Reference plugins (slack, docker, …)
charts/      Helm chart (server + agents)
examples/    Sample .gocdnext.yaml files
docs/        Architecture & pipeline spec
```

## Cloud dev (Codespaces / Gitpod)

Zero local setup + **public URLs** so GitHub webhooks can actually land
during development — key for exercising the `auto_register_webhook`
+ push → run flow end-to-end.

- Click **Open in GitHub Codespaces** or **Open in Gitpod** above.
- The devcontainer / `.gitpod.yml` bootstrap seeds `.env`, installs
  `air` + `goose`, `pnpm install`s the web, and builds the plugin
  images (`gocdnext/node`, etc.).
- Run `make dev` to bring up postgres + server + agent + web with
  hot reload.
- **Webhook testing**:
  - *Gitpod*: port `8153` is flagged `visibility: public` in
    `.gitpod.yml`; GitHub can POST directly at
    `https://8153-<workspace>.<region>.gitpod.io/api/webhooks/github`.
  - *Codespaces*: forward port `8153` as **Public**
    (`gh codespace ports visibility 8153:public` or right-click the
    port in VS Code). The post-create already sets
    `GOCDNEXT_PUBLIC_BASE` to the workspace URL.

See [docs/cloud-dev.md](docs/cloud-dev.md) for the full workflow,
port map, cost budgets, and troubleshooting.

## Quick start (dev)

```bash
# 1. start postgres + minio
make up

# 2. apply migrations
export GOCDNEXT_DATABASE_URL='postgres://gocdnext:gocdnext@localhost:5432/gocdnext?sslmode=disable'
make migrate-up

# 3. build everything
make build

# 4. run server + agent (dev mode)
./bin/gocdnext-server &
GOCDNEXT_SERVER_ADDR=localhost:8154 GOCDNEXT_AGENT_TOKEN=dev-token ./bin/gocdnext-agent &

# 5. validate all pipelines in a repo's .gocdnext/ folder
./bin/gocdnext validate examples/simple
```

## Pipeline spec

Pipelines live in a **`.gocdnext/` folder** at the repo root. One file per
pipeline, multiple pipelines per repo. See [docs/pipeline-spec.md](docs/pipeline-spec.md)
for the full reference.

```
your-repo/
├── .gocdnext/
│   ├── build.yaml          ← pipeline "build"
│   ├── deploy-api.yaml     ← pipeline "deploy-api"
│   └── deploy-worker.yaml  ← pipeline "deploy-worker"
└── src/...
```

Minimal file:

```yaml
# .gocdnext/build.yaml
name: build                      # optional; filename used as fallback

materials:
  - git:
      url: https://github.com/org/repo
      branch: main
      on: [push, pull_request]
      auto_register_webhook: true

stages: [compile, test]

jobs:
  compile:
    stage: compile
    image: golang:1.23
    script: [go build ./...]

  test:
    stage: test
    image: golang:1.23
    needs: [compile]
    script: [go test ./...]
```

## Install with Helm

Each `vX.Y.Z` tag publishes the chart to two registries — pick whichever
your tooling prefers.

**Classic Helm repo (gh-pages)**:

```bash
helm repo add gocdnext https://klinux.github.io/gocdnext
helm repo update
helm install gocd gocdnext/gocdnext --version 0.1.0 \
  --set devDatabase.enabled=true \
  --set agent.tokenSecret.value="$(openssl rand -hex 32)" \
  --set webhookToken.value="$(openssl rand -hex 32)" \
  --set secretKey.value="$(openssl rand -hex 32)" \
  --set artifactsSignKey.value="$(openssl rand -hex 32)"
```

**OCI** (Helm 3.8+):

```bash
helm install gocd oci://ghcr.io/klinux/charts/gocdnext --version 0.1.0 \
  --set devDatabase.enabled=true \
  ...
```

The container images (`ghcr.io/klinux/gocdnext-{server,agent,web}`) are
multi-arch (amd64 + arm64) and tagged `latest` on every push to `main`,
plus `vX.Y.Z` / `X.Y` / `X` on tag releases.

## Architecture

See [docs/architecture.md](docs/architecture.md) for the design. TL;DR:

```
  ┌─────────┐  webhook    ┌─────────────┐   gRPC stream   ┌───────────┐
  │ GitHub  │ ──────────▶ │   server    │ ◀──────────────▶│  agent(s) │
  └─────────┘             │  (Go,chi,   │                 │  (Go,     │
                          │   gRPC,     │                 │  container│
  ┌─────────┐    HTTP     │   sqlc)     │                 │  runtime) │
  │  web UI │ ──────────▶ │             │                 └───────────┘
  │ Next.js │             └──────┬──────┘
  └─────────┘                    │
                           ┌─────▼──────┐
                           │ PostgreSQL │
                           └────────────┘
```

## Roadmap (high-level)

| Phase | Weeks | Delivers |
|-------|-------|----------|
| 0 — Foundation     | 1–2  | Scaffold, proto, migrations, docker-compose  |
| 1 — MVP pipeline   | 3–6  | Webhook GitHub, parse YAML, run 1-stage job  |
| 2 — Diferencial    | 7–10 | Upstream material, fanout, VSM, PR native    |
| 3 — Validação      | 11–14| Rodar 3–5 pipelines reais internos           |
| Gate               | —    | Decidir: abrir / continuar interno / pivotar |

## License

Apache 2.0 — even though it's internal for now, we want the option to open it.
