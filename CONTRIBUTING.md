# Contributing

## Dev setup

```bash
# prereqs: go 1.23, docker, buf (for proto), goose (for migrations), node 20+

git clone <this-repo>
cd gocdnext
make up                     # postgres + minio
make migrate-up             # applies server/migrations/*.sql
make build
make test
```

## Layout

- `server/` — Go module for the control plane
- `agent/` — Go module for the runner
- `cli/` — Go module for the `gocdnext` CLI
- `web/` — Next.js app
- `proto/` — protobuf contracts; run `make proto` after changes
- `plugins/` — reference plugins (each is its own Go module)

Cross-module changes use `go.work` (root). Each module has its own `go.mod` so
they can be released independently.

## Conventions

- SQL migrations are goose-style (`-- +goose Up` / `Down`), one file per change.
- No ORM — use sqlc. Query files live next to migrations.
- Log with `slog` (JSON handler). No `fmt.Println` in non-main packages.
- Errors wrapped with `fmt.Errorf("%w", err)`.
- Tests use real Postgres via testcontainers where integration is needed.

## Commit style

Conventional Commits: `feat:`, `fix:`, `docs:`, `chore:`, `refactor:`, `test:`.
Subject ≤ 72 chars. Body explains *why*.
