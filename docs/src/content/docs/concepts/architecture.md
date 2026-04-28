---
title: Architecture deep-dive
description: How the control plane, the agents, the scheduler, and the database fit together.
---

gocdnext is three Go binaries + a Postgres + an artefact backend.
This page walks the moving parts so an operator can debug or
extend without reading the whole codebase.

## Components

```
┌─────────────────────────────────────────────────────────────┐
│                     Control plane (server)                  │
│  ┌────────────┐  ┌────────────┐  ┌──────────────────────┐   │
│  │ HTTP API   │  │ gRPC stream│  │ Scheduler goroutine  │   │
│  │ :8153      │  │ :8154      │  │ LISTEN run_queued    │   │
│  └────────────┘  └────────────┘  └──────────────────────┘   │
│         │              │                      │              │
│         └──────────────┴──────────────────────┘              │
│                        │                                     │
│              ┌─────────────────────┐                         │
│              │   Postgres          │                         │
│              │   pgxpool +         │                         │
│              │   LISTEN/NOTIFY     │                         │
│              └─────────────────────┘                         │
└─────────────────────────────────────────────────────────────┘
                        ▲                       ▲
                        │ HTTP + Postgres TLS   │ gRPC bidi stream
                        │                       │
        ┌───────────────┴───────┐       ┌───────┴────────────┐
        │  web (Next.js)        │       │  agent (Go)        │
        │  Server Actions       │       │  Docker / K8s      │
        │  RSC fetch            │       │  runtime engine    │
        └───────────────────────┘       └────────────────────┘
                                                 │
                                                 │ runs jobs as
                                                 ▼
                                        ┌────────────────────┐
                                        │  plugin container  │
                                        │  (gocdnext/go,     │
                                        │   /node, /helm…)   │
                                        └────────────────────┘
```

### `server` — the control plane

One Go binary, three roles:

- **HTTP** on `:8153` — REST API, webhooks, SSE log stream.
- **gRPC** on `:8154` — agent registration + bidirectional log
  stream.
- **In-process scheduler** — single goroutine listening on
  `pg_notify('run_queued', ...)`, picking up runs as they're
  created and dispatching jobs to free agents.

Co-located in one binary because the latency between webhook →
run created → job dispatched matters more than the deployment
flexibility of separating them. A push-driven build wants to
start running within seconds.

### `agent` — the runner

One Go binary per agent host. Maintains a long-lived gRPC stream
to the server (single-writer Send invariant; Send + CloseSend
share the same goroutine). On `JobAssignment`:

1. Clones the materials.
2. Starts the plugin container (Docker engine OR Kubernetes
   engine, depending on the agent's `GOCDNEXT_AGENT_ENGINE`).
3. Streams stdout/stderr lines back as `LogLine` messages,
   bulk-batched (100 lines / 200 ms).
4. Reports `JobResult` on terminal status.

Agents register at boot via `Register` RPC, get a session token,
hold the stream open. The server's `SessionStore` manages capacity
+ tag-based routing; the scheduler dispatches jobs to whichever
session matches the job's tag requirements + has free slots.

### `web` — the dashboard

Next.js 15 + React 19, App Router, RSC default. Server Actions
hit the platform's HTTP API for mutations; RSC fetch hits it
for reads. Client components use TanStack Query for live
polling + SSE subscription for log tailing.

The web tier is stateless — every request goes to the server's
HTTP API. You can run N replicas behind a load balancer.

### Postgres — the source of truth

Everything else is a cache or a transient. Postgres holds:

- `projects`, `pipelines`, `materials` — pipeline definitions.
- `runs`, `stage_runs`, `job_runs` — run state.
- `log_lines` — log stream (RANGE-partitioned by month).
- `artifacts`, `caches` — backend metadata (the bytes are in
  the artefact backend).
- `secrets` (when `backend=db`) — AES-256-GCM-encrypted values.
- `agents`, `runner_profiles` — agent fleet state.
- `users`, `groups`, `audit_events` — RBAC + audit.

`pg_notify` + `LISTEN` is what wakes the scheduler; the channels
are `run_queued` (new run created) and `run_done` (terminal flip).
The scheduler holds one dedicated `pgx.Conn` for `LISTEN`; the
rest of the platform shares a `pgxpool.Pool`.

### Artefact backend — the bytes layer

Anything that's not metadata: artefact files, cache tarballs,
cold-archived log gzips. Three backends:

- `filesystem` — local PVC. Default. Single-server only.
- `s3` — AWS S3, MinIO, R2, any S3-compatible.
- `gcs` — Google Cloud Storage.

The platform's `internal/artifacts` package abstracts these
behind the same `Store` interface. Switching backends is a
config change + a one-shot rsync of existing data — no schema
migration.

## Request flows

### Webhook → run created

1. GitHub POSTs to `/api/v1/webhook/github`.
2. HMAC validated against the SCM source's secret.
3. Push event extracted: `(repo, branch, sha)`.
4. `store.InsertModification` upserts a row in `modifications`
   (idempotent — same `(material_id, sha)` is a no-op).
5. If a new modification was created, `store.CreateRunFromModification`
   inserts `runs` + `stage_runs` + `job_runs` in one transaction
   AND `pg_notify('run_queued', <run_id>)` within the same tx.
6. The scheduler's `LISTEN` goroutine wakes up, picks up the run,
   dispatches jobs (see below).

The whole flow is webhook → run dispatched in under a second on
a healthy install.

### Job dispatch

1. Scheduler reads the active stage for the run (lowest ordinal
   with `queued`/`running` jobs).
2. Atomically claims a `queued` job: `UPDATE job_runs SET
   status='running', agent_id=$1 WHERE id=$2 AND status='queued'`.
   If 0 rows affected, another scheduler tick beat us — fine, move
   on.
3. Constructs a `JobAssignment` proto with the materials, env,
   secrets, plugin spec.
4. Looks up an idle session in `SessionStore` matching the job's
   tags + capacity.
5. Pushes the assignment onto the session's outbound channel.
6. Send-pump goroutine dequeues + writes to the gRPC stream.

If no session matches (no agent with required tags or all are
full), the job stays `queued`; the next scheduler tick re-tries.

### Log stream

1. Agent's runner writes a line to its in-process channel.
2. The send-pump batches lines (100 / 200 ms) into a single
   `AgentMessage{Log: ...}` proto with multiple `LogLine` entries.
3. Server's gRPC handler unpacks the batch, calls
   `store.BulkInsertLogLines` (multi-VALUES INSERT, ON CONFLICT
   on the triple key).
4. After the DB write, the server publishes each line to the
   in-process `logstream.Broker` — SSE subscribers fan out.

The DB lag behind the SSE fan-out is up to flushEvery (~200ms);
acceptable per the docs convention.

### Cold-archive flow

When a job hits a terminal status:

1. The agent_service's `handleJobResult` calls
   `maybeEnqueueArchive` which folds the global policy + project
   override.
2. If archiving is on for this job, `archiver.Submit(jobRunID)`
   pushes onto a queue.
3. The archiver's worker pool picks up the queue:
   - Reads all log_lines for the job.
   - Streams gzipped lines into a buffer.
   - Uploads via `artifacts.Store.Put`.
   - Stamps `job_runs.logs_archive_uri`.
   - DELETEs from `log_lines`.
4. Read path: `getRunDetail` checks `logs_archive_uri` first;
   falls back to `log_lines` for jobs without an archive.

Failures at any stage leave the row in place; the retention
sweeper's reconcile pass picks up stragglers (re-submit jobs
without URI; DELETE log_lines for jobs WITH URI).

## Concurrency invariants

- **Single-writer on gRPC `Send`** — the agent's send-pump is
  the only goroutine that writes to the stream. `Recv` runs in
  parallel (different direction = safe).
- **Scheduler is single-goroutine** within a server replica.
  `FOR UPDATE SKIP LOCKED` on the dispatch query lets multiple
  replicas coordinate without conflict.
- **Job claim is atomic** — `UPDATE … WHERE status='queued'`
  with the ID predicate. Lost the race? Move on.
- **Log batch insert is single-job-per-batch** — the bulk insert
  query is fine with mixed jobs in one batch but the ON CONFLICT
  semantics are simpler when batches are homogeneous; the
  agent's send-pump groups by `(jobRunID)` accordingly.

## Scaling notes

- **Server**: stateless. Run N replicas. They coordinate via
  Postgres (LISTEN/NOTIFY + atomic UPDATEs). Tested up to 10
  replicas. Bottleneck above that is Postgres connection count
  + LISTEN fanout.
- **Agent**: scales horizontally. Each agent has its own
  capacity (`GOCDNEXT_AGENT_CAPACITY`). Tag-based routing
  partitions work.
- **Postgres**: vertical scaling matters most. Heavy log
  insert load benefits from `wal_level=logical` + tuning the
  shared buffer + WAL sender count. Partitioned `log_lines` is
  the single biggest scalability win — keeps the heap from
  becoming the bottleneck.
- **Web**: stateless Next.js. N replicas behind a load balancer.

## Where to look in the code

| Component | Path |
|---|---|
| HTTP routes | `server/cmd/gocdnext-server/main.go` |
| Webhook handlers | `server/internal/webhook/<provider>/` |
| Pipeline parser | `server/internal/parser/` |
| Scheduler | `server/internal/scheduler/` |
| gRPC service | `server/internal/grpcsrv/` |
| Store (Postgres ops) | `server/internal/store/` |
| Plugin catalog | `server/internal/plugins/` |
| Agent runtime | `agent/internal/runner/` + `agent/internal/engine/` |
| Web pages | `web/app/` |
| Web components | `web/components/` |
| Server Actions | `web/server/actions/` |

The codebase keeps each file under ~400 lines (per `CLAUDE.md`
house rules), so navigating it is quick. Read the package's
top-level comment in any file you open — they're written for
the next reader, not the compiler.
