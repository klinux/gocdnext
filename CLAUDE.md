# gocdnext — Development Guide

Engineering rules for this repository. Applies to any code landing in `server/`, `agent/`, `cli/`, `plugins/`, `web/`, `proto/`.

Architectural decisions already settled (stack, `.gocdnext/` layout, LISTEN/NOTIFY, sqlc, gRPC bidi, plugin-container) live in `docs/` and are not relitigated without a new technical reason.

## Non-negotiable rules

- **TDD always.** Red test → minimal code → green → refactor. No PR without a new test or one covering the changed path.
- **~400 lines per file.** Past that, split. Applies to `.go`, `.ts`, `.tsx`, `.sql`. Tests may exceed when the suite is a single cohesive whole.
- **shadcn for UI.** Every visual component in `web/` comes from shadcn/ui. Don't roll your own Button, Dialog, Input when the shadcn equivalent exists. Customise via `className` and variants.

## Implementation posture (senior dev)

Every PR — hotfixes included — passes through these lenses. "Happy-path passes" is not a definition of done.

- **Corner cases first.** Before writing the function, list: empty input, nil input, oversized value, duplicates, unicode/case, races with other goroutines, ctx canceled mid-call, missing dependency. Each item becomes a test or a comment explaining why it can't happen.
- **Security is not optional.**
  - Substitution/templating: never pass user input straight to shell, SQL, exec, log, or error message without sanitising. Errors about unresolved references cite the reference **name**, never the **value** of something else.
  - Secrets: the resolved value goes into `LogMasks` in the same step it's injected into env/settings. Forgetting to add it to the mask leaks it to the log.
  - Substitution is **single-pass**. No expanding the result of one substitution (prevents recursive `${{ X }}` → `${{ Y }}` loops and leak-via-chain).
  - Parsing limits (regex, YAML recursion, JSON depth) are explicit. No `\w+` without a bound, no unmarshalling a blob without `MaxBytes`.
  - Credential comparisons use `subtle.ConstantTimeCompare`. HMAC and tokens never with `==`.
- **Performance measured, not guessed.**
  - Regex compiled once in `init`, not per call.
  - Allocations in hot loops (scheduler dispatch, log streaming, webhook hot path) minimised — `make([]T, 0, n)` when `n` is known.
  - Touched a hot path? Run `go test -bench` before/after. Put the allocs diff in the PR description.
  - New query with a JOIN or subquery: `EXPLAIN ANALYZE` in the migration, or a comment justifying "OK at < N rows".
- **Failures loud, not silent.** A swallowed error (`_ = ...`) needs a comment explaining why it's fine. Default is propagate + structured log.
- **Defence in depth.** Same validation on server and agent (don't trust upstream sanitisation). Same invariant checked at parse, apply, and dispatch.

## Go (server, agent, cli, plugins)

- **Lint**: `golangci-lint` with `.golangci.yml` at the root. Active presets: `errcheck`, `govet`, `staticcheck`, `gosec`, `revive`, `gocyclo`, `ineffassign`, `unused`. CI fails on the first warning.
- **Race detector mandatory**: `go test -race ./...` in CI. Don't disable locally.
- **Context is always the first argument** for any function that does I/O, calls gRPC, or touches the database. `context.Background()` only in `main` and tests.
- **Errors wrapped with `%w`**: `fmt.Errorf("parse pipeline %s: %w", name, err)`. Assert with `errors.Is` / `errors.As`.
- **Structured logging** with `slog`. No `fmt.Println` or `log.Printf` in production code. Consistent fields: `pipeline`, `job`, `agent_id`, `run_id`.
- **Table-driven tests** as the default:
  ```go
  tests := []struct{ name string; in X; want Y }{...}
  for _, tt := range tests { t.Run(tt.name, func(t *testing.T) {...}) }
  ```
- **Postgres integration uses `testcontainers-go`**, never mocks. If a test needs the DB, it spins up a real container.
- **Package names**: lowercase, single word, no underscore, no plural. `pipeline`, not `pipelines` or `pipeline_parser`.
- **sqlc generates into `internal/db/`**. Generated code is never edited by hand.

## Frontend (web/)

Frontend-specific rules (Next.js 15, RSC, Server Actions, shadcn, Tailwind, Zod, Biome, testing) live in [web/CLAUDE.md](web/CLAUDE.md). Claude Code loads hierarchically — when working in `web/`, both files apply.

## Proto / gRPC contracts

- **`buf`** manages proto. `buf.yaml` + `buf.gen.yaml` at the root.
- **Lint in CI**: `buf lint` and `buf breaking --against '.git#branch=main'`. A breaking change forces a package version bump (`v1` → `v2`).
- **Generated code is never edited.** Regenerate with `buf generate`. Output in `proto/gen/go` and `proto/gen/ts`.
- **Contracts live in `proto/gocdnext/v1/`.** New service = new file.

## Git, commits, and CI

- **Conventional Commits** mandatory: `feat(scope):`, `fix(scope):`, `chore:`, `docs:`, `test:`, `refactor:`. Scope optional but recommended.
- **Pre-commit hook** via lefthook. Runs: `gofmt`, `golangci-lint run --fast`, `buf lint`, `tsc --noEmit`, affected fast tests.
- **PRs small and focused.** One PR = one feature/fix. A large refactor lives in a PR separate from the feature.
- **CI GitHub Actions**: lint → build → unit tests → integration tests (with containers) → e2e tests (when they exist).
- **Migrations**: goose, forward-only. Don't create a `.down.sql` that runs in production. Rollback = a corrective new migration.
- **Secrets**: `.env.example` committed, `.env` in `.gitignore`. No credentials in pipeline YAML, Dockerfile, or code.

## Dependencies

- **Renovate/Dependabot** bumps deps weekly. A human reviews and merges.
- **A new dep needs justification in the PR**: "why not stdlib or what we already have?". Avoid library sprawl.
- **Pin major versions** in `go.mod` and `package.json`. Minor/patch may float.

## Observability (since Phase 1)

- **OpenTelemetry traces** on server and agent from the very first endpoint/stream. Retrofitting later is expensive.
  - Named spans: `pipeline.parse`, `job.schedule`, `agent.stream.recv`, `webhook.receive`.
  - Propagation via gRPC context and HTTP headers.
- **Prometheus metrics** at `/metrics`:
  - Jobs: `gocdnext_jobs_scheduled_total`, `gocdnext_jobs_running`, `gocdnext_job_duration_seconds`.
  - Queue: `gocdnext_queue_depth{stage}`.
  - gRPC: latency, RPS, error rate per method.
- **Correlated logs** with `trace_id` and `span_id` via an OTel-aware `slog` handler.

## Directory conventions

```
server/
  cmd/gocdnext-server/   # main
  internal/
    api/      # HTTP handlers + Server Actions-facing
    grpc/     # gRPC services
    db/       # sqlc generated
    domain/   # pure business types
    pipeline/ # parser, validator, scheduler
  migrations/ # goose

agent/
  cmd/gocdnext-agent/
  internal/
    runtime/  # docker/k8s executor
    stream/   # gRPC client
    plugin/   # plugin runner

web/
  src/
    app/              # Next.js App Router
    components/ui/    # shadcn
    components/       # app components
    lib/              # utils
    server/           # Server Actions

proto/gocdnext/v1/
plugins/<name>/
```

- `internal/` is module-private. No cross-imports between `internal/` of different modules — use proto or a public package.

## Before opening a PR

- Tests green locally, with the race detector.
- Lint clean (`make lint`).
- `buf lint` and `buf breaking` clean.
- No orphan `TODO` (if it stays, link an issue).
- No file > 400 lines (test files accepted).
- Commit message follows Conventional Commits.
