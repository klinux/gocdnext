---
title: Local dev
description: Run gocdnext from source on your laptop.
---

`make dev` boots the whole platform on your machine: control plane
(`server`), an agent, the web dashboard, and a Postgres in Docker.
Useful for iterating on plugins, parser changes, or new pipeline
recipes before they hit the cluster.

## Prerequisites

- Go 1.25 (the workspace is on `go 1.25.0`).
- pnpm 9+ for the web frontend.
- Docker (the agent's default runtime spawns containers).
- `goose` for migrations: `go install github.com/pressly/goose/v3/cmd/goose@latest`.
- `sqlc` for query regeneration: `go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest`.
- `buf` for proto changes: `go install github.com/bufbuild/buf/cmd/buf@latest`.

## First run

```bash
git clone https://github.com/klinux/gocdnext.git
cd gocdnext
cp .env.example .env
make dev
```

What `.env.example` covers:

- `GOCDNEXT_DATABASE_URL` — points at the postgres-dev container.
- `GOCDNEXT_DOCKER_PULL_POLICY=always` — recommended in dev so plugin
  image edits show up without manual `docker pull`.
- `GOCDNEXT_AGENT_NAME=dev`, `GOCDNEXT_AGENT_TOKEN=dev-token` —
  pre-provisioned in the dev seed.

Once `make dev` settles:

- Server on `http://localhost:8153`
- Agent registered (check `/agents` in the dashboard)
- Web on `http://localhost:3000`

## Working on a pipeline locally

Create `.gocdnext/hello.yaml`:

```yaml
name: hello
when:
  event: [push, pull_request]
stages: [smoke]
jobs:
  greet:
    stage: smoke
    image: alpine:3.20
    script:
      - echo "hello from $(uname -sr)"
```

Apply it:

```bash
gocdnext apply --slug demo --name "Demo project" .
```

Trigger a run from the dashboard or push a commit if you've wired the
`scm_source`. Logs stream live via SSE; reloads keep working through
the cursor-paginated read API.

## Working on a plugin

Plugins live in `plugins/<name>/` — Dockerfile, entrypoint, and a
`plugin.yaml` manifest. The thin shim pattern from
`plugins/go/entrypoint.sh` is the simplest reference:

```sh
#!/bin/sh
set -eu
cd /workspace
[ -n "${PLUGIN_WORKING_DIR:-}" ] && cd "${PLUGIN_WORKING_DIR}"
echo "==> go ${PLUGIN_COMMAND}"
exec go ${PLUGIN_COMMAND}
```

Local iteration loop:

```bash
docker build -t gocdnext-plugin-myplugin:dev plugins/myplugin
# Edit plugins/myplugin/plugin.yaml so the catalog picks up your
# inputs schema. The server reloads the catalog on restart only —
# `make dev-restart-server` after a manifest edit.
```

To exercise the plugin in a real pipeline locally, push your dev
image to a registry the agent can reach (or load it directly into
the agent's Docker daemon) and reference it via:

```yaml
uses: gocdnext-plugin-myplugin:dev
```

## Tests

```bash
make test                # full suite, race detector on, includes containers
make lint                # golangci-lint on every module + buf lint
make web-build           # next.js production build
```

Database integration tests use testcontainers-go — they spin up a
fresh Postgres per test binary. First invocation pulls the postgres
image (~200 MB); subsequent runs reuse it.

## Cleaning up

```bash
make dev-down            # stop + drop the postgres-dev container
docker volume prune      # if you want to wipe the dev DB volume
```
