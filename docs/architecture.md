# Architecture

## Processes

### `gocdnext-server` (control plane)

- **HTTP API** (`:8153`) — REST for UI and external tools. Chi router.
- **gRPC** (`:8154`) — bidirectional stream for agents (`AgentService`).
- **Webhook receiver** — mounted under `/api/webhooks/{github,gitlab,bitbucket}`.
- **Scheduler** — consumes PostgreSQL `LISTEN/NOTIFY` channels; decides which jobs
  are ready (all `needs` satisfied, stage unlocked) and dispatches to connected
  agents.
- **Persistence** — PostgreSQL 16 via pgx + sqlc (no ORM; SQL stays real).

### `gocdnext-agent`

- Starts outbound gRPC connection to server (NAT-friendly, no inbound port).
- Registers with token + tags (`linux`, `docker`, `gpu`, custom labels).
- Receives `JobAssignment`, runs each task:
  - `script:` → `sh -c` inside the job image
  - plugin step → new container with `PLUGIN_*` env vars (Woodpecker contract)
- Streams logs back line-by-line (seq-numbered, idempotent).
- Supports cancellation (kills container on `CancelJob`).

### `web` (Next.js 15)

- App Router + Server Components.
- TanStack Query for client state.
- Streams logs via SSE (server → browser).
- Renders VSM with [@xyflow/react](https://reactflow.dev/).

## Data flow: webhook → pipeline run

```
1. GitHub POST /api/webhooks/github
2. Server validates HMAC (per-material secret, falls back to global)
3. Match webhook payload → material(s) by URL + branch
4. INSERT modifications, NOTIFY new_modification
5. Scheduler picks up, creates runs + stage_runs + job_runs (status=queued)
6. For each ready job, find capable agent (tags match), dispatch JobAssignment
7. Agent clones materials, executes tasks, streams LogLine + JobProgress
8. Agent sends JobResult (success/failure + artifacts)
9. Server updates job_runs; if stage complete, advances to next stage
10. On pipeline success: UPSERT modification into any downstream "upstream" material
    → triggers fanout (step 4 repeats for downstreams)
```

## Why these choices

- **Postgres LISTEN/NOTIFY** over Kafka/NATS for MVP: 1 less dep, fine for
  internal scale (<10k jobs/day). Switch later if needed.
- **sqlc** over ORMs: queries are reviewed SQL, tests use real Postgres.
- **gRPC bidirectional** over HTTP polling: lower latency, clean cancellation,
  single socket per agent.
- **Containers for everything** (including plugins): zero SDK, any language.

## What's explicitly NOT here

- **Multi-tenant / RBAC**: out of scope for internal MVP.
- **HA / leader election**: single server; restart is the recovery plan.
- **Plugin marketplace**: plugins live in any container registry.
- **DSL / programmable pipelines**: YAML only. `run-local` covers dev loops.
