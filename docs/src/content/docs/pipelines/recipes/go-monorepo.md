---
title: Go monorepo (test + build)
description: A multi-module Go monorepo with race-detector tests, build artefacts, and warm caches across runs.
---

This recipe is what gocdnext itself uses for [`.gocdnext/ci-server.yaml`](https://github.com/klinux/gocdnext/blob/main/.gocdnext/ci-server.yaml) — three modules
(server, agent, cli), each with its own tests and a compiled binary,
sharing the Go module cache and build cache between runs so cold
boots are the exception, not the rule.

## Layout assumed

```
repo/
├── go.work
├── server/
│   ├── go.mod
│   └── cmd/myapp-server/...
├── agent/
│   └── go.mod
└── cli/
    └── go.mod
```

## The pipeline

```yaml title=".gocdnext/ci-server.yaml"
name: ci-server

when:
  event: [push, pull_request]

stages: [lint, test, build]

jobs:
  vet:
    stage: lint
    uses: gocdnext/go@v1
    with:
      working-dir: server
      command: vet ./...

  unit:
    stage: test
    uses: gocdnext/go@v1
    needs: [vet]
    docker: true                    # testcontainers-go needs the host docker.sock
    with:
      working-dir: server
      command: test -race ./...
    cache:
      - key: go-server-${CI_COMMIT_BRANCH}
        paths:
          - .go-mod
          - .go-cache

  compile:
    stage: build
    uses: gocdnext/go@v1
    needs: [unit]
    with:
      working-dir: server
      command: build -o ../bin/myapp-server ./cmd/myapp-server
    artifacts:
      paths: [bin/myapp-server]
```

Three things worth highlighting.

### `docker: true` for integration tests

When tests use `testcontainers-go` (or anything else that needs to
spawn sibling containers), the job needs the host's docker socket
mounted in. `docker: true` does that, plus wires
`TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE` and a `host.docker.internal`
gateway alias so `testcontainers` finds the daemon deterministically
on Linux.

### Cache `.go-mod` and `.go-cache`, not `~/go`

The `gocdnext/go` plugin redirects `GOMODCACHE` and `GOCACHE` to
workspace-local directories so the platform's `cache:` block can
tar them across runs. Default Go locations sit under `$HOME` and the
agent can't see those.

A warm run (lockfile unchanged, code touched) typically drops a
multi-module compile from minutes to seconds — the analyzer skips
unchanged packages because the build cache hits.

### `working-dir: server` instead of one job per dir

When modules don't share state, you can spin three parallel jobs
each in its own `working-dir`. We do that for ci-server / ci-agent /
ci-cli in separate pipelines so an agent-only PR doesn't wait on the
server matrix.

## Adding a lint pipeline

Cross-module lint earns its own pipeline so a lint failure surfaces
quickly without blocking tests:

```yaml title=".gocdnext/lint.yaml"
name: lint
when:
  event: [push, pull_request]
stages: [lint]
jobs:
  golangci:
    stage: lint
    uses: gocdnext/golangci-lint@v1
    cache:
      - key: golangci-${CI_COMMIT_BRANCH}
        paths: [.go-mod, .go-cache, .golangci-cache]
    with:
      args: ./server/... ./agent/... ./cli/...

  buf:
    stage: lint
    uses: gocdnext/buf@v1
    with:
      working-dir: proto
      command: lint
```

Same `cache:` trick — `golangci-lint` redirects its own cache plus
`GOMODCACHE`/`GOCACHE` into the workspace so warm linting drops to
seconds. First run will still take 5–10 minutes (compile every
package + run every analyzer); from there it's incremental.

## Triggering on relevant changes only

If the monorepo also has a `web/` (TypeScript) and you don't want a
JS edit to fire the Go pipelines, use `when.paths`:

```yaml
when:
  event: [push, pull_request]
  paths:
    - "server/**"
    - "go.work"
    - ".gocdnext/ci-server.yaml"
```

This is parsed as include-list; runs are skipped when none of the
push's changed files match.
