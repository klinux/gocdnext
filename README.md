# gocdnext

> Modern CI/CD orchestrator. Cherry-picks the good ideas from **GoCD** (VSM,
> fanout, pipeline dependencies, stage/job model), **Woodpecker** (plugin =
> container), and **GitLab CI** (stages, rules, needs, matrix, extends).
> Written in Go. UI in Next.js. Container-native. Webhook-first.

Status: **active development** — v0.x, minor bumps may carry breaking
changes until 1.0. Public repo, shipping monthly.

📚 **Docs**: <https://klinux.github.io/gocdnext/docs/>

[![Open in GitHub Codespaces](https://github.com/codespaces/badge.svg)](https://codespaces.new/klinux/gocdnext)
[![Open in Gitpod](https://img.shields.io/badge/Gitpod-ready--to--code-908a85?logo=gitpod)](https://gitpod.io/#https://github.com/klinux/gocdnext)

![Dashboard](docs/public/screenshots/01-dashboard.png)

## Why another CI tool?

We loved GoCD's model (explicit stage → job → task, dependency materials, VSM)
but hated the stack: Java/Spring/Hibernate, XML config, poll-first, Rails UI,
no plugin marketplace. This is what GoCD would look like if we started today.

Differentiators vs. GitHub Actions / Tekton / Woodpecker:

- **Upstream material** — `pipeline B` waits for `pipeline A.stage X` to pass
  *with the same commit SHA*, with automatic fanout across N downstreams.
- **Value Stream Map (VSM)** — visualize the graph of pipelines + materials.
- **Webhook-first**, polling only as fallback. **Auto-register webhook** on
  GitHub / GitLab / Bitbucket when you create a git material.
- **Plugin catalog** — 60+ reference plugins (build/test/scan/sign/deploy/
  migrate/notify), each shipped as a versioned container image with a typed
  input contract.
- **Kubernetes-native runtime** — pod-per-job execution with runner profiles
  (K1–K4), `node_selector`/`tolerations` scheduling, or classic Docker on
  the agent host.
- **Keyless cloud auth** — jobs mint short-lived OIDC `id_tokens:` (with
  JWKS discovery) and exchange them for cloud credentials via workload
  identity federation — no static service-account keys in secrets.
- **Deployment tracking + rollback** — `deploy:` markers record
  version-per-environment with history, plus **one-click rollback** that
  reuses the prior revision's immutable outputs.
- **Pipeline services** — sibling service containers (postgres,
  redis, etc.) declared in YAML, reachable by every job via DNS
  alias, and rendered as nodes in the pipeline graph.
- **RBAC + audit log** — admin/maintainer/viewer hierarchy, every mutation
  recorded in `audit_events`.
- **Approval gates** — gate stages on approver groups with quorum (incl.
  PR-label-driven `quorum_by_label`), full audit trail, and zero compute
  held while a gate waits — it's pure DB state.
- **Run it locally** — `gocdnext run-local` executes a pipeline on your
  machine before you push.

## Screenshots

<table>
  <tr>
    <td width="50%">
      <a href="docs/public/screenshots/02-run-detail.png">
        <img src="docs/public/screenshots/02-run-detail.png" alt="Run detail with live logs" />
      </a>
      <p align="center"><sub>Run detail — Jobs / Tests / Artifacts tabs with live log stream</sub></p>
    </td>
    <td width="50%">
      <a href="docs/public/screenshots/03-project-pipelines.png">
        <img src="docs/public/screenshots/03-project-pipelines.png" alt="Project pipelines" />
      </a>
      <p align="center"><sub>Project pipelines with bottleneck pill + stage strip</sub></p>
    </td>
  </tr>
  <tr>
    <td width="50%">
      <a href="docs/public/screenshots/04-vsm.png">
        <img src="docs/public/screenshots/04-vsm.png" alt="Value Stream Map" />
      </a>
      <p align="center"><sub>Value Stream Map — pipelines + materials graph</sub></p>
    </td>
    <td width="50%">
      <a href="docs/public/screenshots/05-plugins-catalog.png">
        <img src="docs/public/screenshots/05-plugins-catalog.png" alt="Plugin catalog" />
      </a>
      <p align="center"><sub>Plugin catalog — auto-generated from <code>plugin.yaml</code></sub></p>
    </td>
  </tr>
</table>

## Repo layout

```
server/      Go control plane: HTTP API, gRPC for agents, scheduler, webhooks
agent/       Go agent: pulls jobs, runs containers (docker or k8s), streams logs
cli/         gocdnext CLI: validate, run-local, apply, secret, login, admin
web/         Next.js 15 UI (App Router, RSC, Server Actions, shadcn)
proto/       gRPC / protobuf contracts (managed by buf)
plugins/     Reference plugins — 60+ images (build/test/scan/sign/deploy/migrate/notify)
charts/      Helm chart (server + agents, single-host Ingress / Gateway API)
examples/    Sample .gocdnext/ pipeline files
docs/        Starlight docs site (concepts, recipes, reference, operate guide)
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

The fast path uses `make dev` to bring everything up with hot reload —
postgres + minio + server + agent + web, behind a single foreground
process. Ctrl+C tears it all down.

```bash
# 1. one-shot env scaffold (.env + tools — air, goose, sqlc, buf)
make env-setup

# 2. bring up the full stack with hot reload
make dev
```

That's it. The UI lands on <http://localhost:3000>, the API on `:8153`,
the agent connects via gRPC on `:8154`.

If you want the pieces separately (e.g. to attach a debugger):

```bash
make db-up                   # postgres + minio only
make migrate-up              # apply migrations
make build                   # compile server + agent + cli
./bin/gocdnext-server &
GOCDNEXT_SERVER_ADDR=localhost:8154 GOCDNEXT_AGENT_TOKEN=dev-token ./bin/gocdnext-agent &
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
helm install gocd gocdnext/gocdnext --version 0.39.3 \
  --set devDatabase.enabled=true \
  --set agent.tokenSecret.value="$(openssl rand -hex 32)" \
  --set webhookToken.value="$(openssl rand -hex 32)" \
  --set secretKey.value="$(openssl rand -hex 32)" \
  --set artifactsSignKey.value="$(openssl rand -hex 32)"
```

**OCI** (Helm 3.8+):

```bash
helm install gocd oci://ghcr.io/klinux/charts/gocdnext --version 0.39.3 \
  --set devDatabase.enabled=true \
  ...
```

Check the [latest release](https://github.com/klinux/gocdnext/releases)
for the current `vX.Y.Z` — both registries publish on every tag.

The container images (`ghcr.io/klinux/gocdnext-{server,agent,web}`) are
multi-arch (amd64 + arm64) and tagged `latest` on every push to `main`,
plus `vX.Y.Z` / `X.Y` / `X` on tag releases.

## Architecture

See [docs/architecture.md](docs/architecture.md) for the design.

![Architecture](docs/public/imgs/architecture.png)

## What's shipped (v0.39.3)

- **Pipeline core** — `.gocdnext/` folder, stage/job/needs/matrix, materials
  (git + upstream), webhook-first ingest with polling fallback.
- **Multi-provider triggers** — push / tag / `pull_request` (GitHub) +
  merge request (GitLab) + PR (Bitbucket) webhooks, with
  `when.event`/`when.paths`/`when.branch` filtering, `[skip ci]` markers,
  and `auto_register_webhook`.
- **Plugin runtime** — versioned container plugins, typed `plugin.yaml`
  contracts, secret-aware env propagation (NAME-only on argv).
- **Plugin catalog** — 60+ reference plugins: build (node/go/maven/gradle/
  python/rust), container (buildx/kaniko/docker-push/cosign/trivy), cloud
  (aws/gcloud/kubectl/helm/kustomize/argocd/terraform), DB migrations
  (flyway/liquibase/goose), quality (sonar/codecov/coveralls/lighthouse-ci/
  gitleaks/golangci-lint), release (release-pr/release-notes/tag), and
  notify (slack/discord/teams/email/matrix).
- **Runtimes** — Docker on the agent host **or** Kubernetes pod-per-job
  with runner profiles (K1–K4), `node_selector`/`tolerations` scheduling,
  and a DinD sidecar for `docker: true` jobs.
- **Keyless cloud auth** — OIDC token issuer: jobs mint short-lived
  `id_tokens:` (RS256, JWKS at `/.well-known/`), exchangeable for cloud
  credentials via workload identity federation (GCP/AWS/Azure/Vault) — no
  static service-account keys.
- **Deployment tracking** — `deploy:` markers record version-per-environment,
  an Environments tab with history, and **one-click rollback** that re-runs
  with the prior revision's immutable outputs.
- **Gated delivery** — release-train flow: a curated release PR (sign-off
  gate) cuts a tag that drives build → stage → **approval gate (promote)** →
  production. Approval gates are pure DB state (approver groups + quorum +
  `quorum_by_label` + audit) — no container held while a gate waits.
- **Artifact + cache** — pluggable storage backends (configurable from
  `/settings/storage`), TTL + per-project + global quotas, container
  layer cache with buildx `cache: bucket` shorthand.
- **Coverage** — per-job coverage reports with `fail_under` gating and a
  delta vs the mainline baseline.
- **CLI** — `validate` + `run-local` (run a pipeline on your machine),
  plus `apply`, `secret`, `login`, and admin commands.
- **RBAC + audit** — admin/maintainer/viewer, `audit_events` table,
  `/settings/users` and `/settings/audit` UI.
- **Supply chain** — cosign keyless image signing + SBOM/SLSA provenance
  attestation in the release path.
- **Operability** — VSM, single-host Ingress / Gateway API in the Helm
  chart, OpenTelemetry traces, Prometheus `/metrics`, `slog` with
  `trace_id`/`span_id` correlation, phase-marked logs + full-log streaming.

## What's open

- **Per-project agent scope / lock** — deferred from the k8s runtime
  rollout.
- **`isolation: per-stage`** — share workspace across jobs in the same
  stage (Woodpecker model).

## License

Apache 2.0 — even though it's internal for now, we want the option to open it.
