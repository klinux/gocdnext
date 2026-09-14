# Contributing to gocdnext

Thanks for your interest! gocdnext is pre-1.0 and moving fast — contributions,
bug reports, and real-world testing are all genuinely useful. This guide gets
you from a fresh clone to a green PR.

By participating you agree to abide by our
[Code of Conduct](CODE_OF_CONDUCT.md). Security issues go through
[SECURITY.md](SECURITY.md), **never** a public issue or PR.

## Ways to help

- **Test it and report back.** Run gocdnext against a real repo and tell us what
  broke or felt wrong — file a [bug](../../issues/new?template=bug_report.yml) or
  start a [discussion](../../discussions).
- **Pick a [`good first issue`](../../issues?q=is%3Aissue+is%3Aopen+label%3A%22good+first+issue%22)**
  or a [`help wanted`](../../issues?q=is%3Aissue+is%3Aopen+label%3A%22help+wanted%22)
  issue. Comment to claim it so we don't double up.
- **Improve the docs.** Fixing a confusing setup step is one of the most
  valuable PRs you can send.
- **Propose a feature** via an issue *before* writing a large change, so we can
  agree on the shape and you don't build something we'd have to reshape in review.

## Dev setup

Prerequisites:

- **Go 1.25+** (the repo pins the toolchain via `GOTOOLCHAIN`)
- **Docker** (Postgres, MinIO, plugin containers)
- **Node 20+** (for `web/`)
- **buf** (only if you touch `proto/`), **goose** (migrations)

```bash
git clone https://github.com/klinux/gocdnext
cd gocdnext

make env-setup      # copy .env.example -> .env
make dev            # boot the full local stack (postgres + server + agent + web), hot reload
# ...or run pieces yourself:
make db-up          # start ONLY postgres
make migrate-up     # apply migrations
make build          # build server, agent, cli
make test           # all Go tests WITH the race detector
make lint           # golangci-lint

make admin-create-user EMAIL=you@example.com ROLE=admin   # seed a login
```

Run `make help` for the full target list. Stop the stack with `make stop`.

## Layout

- `server/` — Go module for the control plane (`internal/api`, `grpc`, `db`,
  `domain`, `pipeline`, migrations)
- `agent/` — Go module for the runner (docker/k8s executor, gRPC stream)
- `cli/` — Go module for the `gocdnext` CLI
- `web/` — Next.js app (App Router, RSC, shadcn/ui)
- `proto/` — protobuf contracts; run `make proto` after changes
- `plugins/` — reference plugins (each its own Go module)
- `docs/` — the docs site (Starlight); user-facing docs live here

Cross-module changes use `go.work` at the root. Each module has its own `go.mod`
so they release independently. `internal/` is module-private — no cross-imports
between different modules' `internal/`; go through proto or a public package.

## Expectations for a PR

We hold every PR — hotfixes included — to the same bar. "Happy path passes" is
not done.

- **TDD.** Write the failing test first, then the minimal code to pass it. No PR
  without a test covering the changed path. Table-driven tests are the default;
  integration tests use a real Postgres via `testcontainers-go`, not mocks.
- **Think about corner cases up front:** empty/nil input, oversized values,
  duplicates, unicode/case, context canceled mid-call, races. Each becomes a
  test or a comment saying why it can't happen.
- **Security is not optional.** Never pass user input straight to shell/SQL/exec
  /log without sanitising; add resolved secret values to `LogMasks` in the same
  step you inject them; use `subtle.ConstantTimeCompare` for tokens/HMAC.
- **~400 lines per file.** Past that, split (`.go`, `.ts`, `.tsx`, `.sql`).
  Cohesive test files may exceed.
- **shadcn for UI.** Don't hand-roll a Button/Dialog/Input that shadcn/ui
  already provides.
- **Fail loud.** A swallowed error (`_ = ...`) needs a comment explaining why.
  Default is propagate + structured `slog` log.

## Conventions

- **Context first.** `context.Context` is the first argument for anything doing
  I/O, gRPC, or DB. `context.Background()` only in `main` and tests.
- **Errors wrapped with `%w`**; assert with `errors.Is` / `errors.As`.
- **Structured logging** with `slog` (JSON). No `fmt.Println`/`log.Printf` in
  non-`main` packages.
- **No ORM** — use sqlc. Query files live in `server/internal/db/queries`;
  generated code in `internal/db/` is never hand-edited.
- **Migrations** are goose-style, **forward-only**. A rollback is a new
  corrective migration, not a `.down.sql` run in production.
- **Proto** is managed by `buf`; regenerate with `make proto`, never hand-edit
  generated output. Breaking changes bump the package version.
- **Frontend rules** live in [web/CLAUDE.md](web/CLAUDE.md) (Next.js 15, Server
  Actions, Zod, Biome).

## Commit & PR style

- **Conventional Commits**: `feat(scope):`, `fix(scope):`, `docs:`, `chore:`,
  `refactor:`, `test:`. Subject ≤ 72 chars; body explains *why*.
- **One PR = one feature/fix.** Keep a large refactor in its own PR, separate
  from a feature.
- Fill in the PR template checklist. CI must be green (lint → build → unit →
  integration with containers) before review.

## Before you open the PR

- Tests green locally **with the race detector** (`make test`).
- Lint clean (`make lint`); `buf lint`/`buf breaking` clean if proto changed.
- `make schema` run if you changed the parser/pipeline schema (CI has a drift
  guard).
- No orphan `TODO` without a linked issue; no file > ~400 lines; no customer
  references or real credentials.

Not sure about something? Open a draft PR or a discussion early — we'd rather
help shape it than see effort go to waste in review.
