# gocdnext

> Modern CI/CD orchestrator. Cherry-picks the good ideas from **GoCD** (VSM,
> fanout, pipeline dependencies, stage/job model), **Woodpecker** (plugin =
> container), and **GitLab CI** (stages, rules, needs, matrix, extends).
> Written in Go. UI in Next.js. Container-native. Webhook-first.

Status: **alpha / internal use**. Not open to public yet.

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
