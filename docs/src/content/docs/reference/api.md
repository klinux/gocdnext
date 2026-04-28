---
title: HTTP API
description: REST endpoints the dashboard, the CLI, and external integrations use to read + write platform state.
---

The control plane exposes a versioned REST API at `/api/v1/`.
Every endpoint requires authentication when `auth.enabled=true` —
either a session cookie (browser flow) or a bearer token
(`Authorization: Bearer <token>`, machine flow). API tokens are
issued via *Settings → API tokens* in the dashboard; they inherit
the role of the user who issues them.

This page is a tour of the endpoints, not exhaustive. The full
surface lives in `server/internal/api/` — search for the route in
`main.go` to find the handler.

## Conventions

- All responses are JSON (`Content-Type: application/json`).
- All times are RFC 3339 with timezone (`2026-04-28T13:00:00Z`).
- IDs are UUIDs (string).
- Errors use HTTP status + a JSON body `{"error": "human-readable message"}`.
- List endpoints support `?limit=N&offset=M` and return
  `{"items": [...], "total": N}`.

## Projects

### `GET /api/v1/projects`

List projects the caller has access to.

```bash
curl -H "Authorization: Bearer $TOKEN" \
  https://ci.example.com/api/v1/projects
```

Response:

```json
{
  "projects": [
    {
      "id": "uuid",
      "slug": "myapp",
      "name": "My App",
      "description": "...",
      "status": "running",
      "latest_run": { "id": "...", "status": "success", ... },
      ...
    }
  ]
}
```

### `GET /api/v1/projects/{slug}`

Fetch one project plus recent runs + pipelines.

`?runs=N` controls how many recent runs are inlined (default 25).

### `POST /api/v1/projects/apply`

The endpoint the CLI's `apply` hits. Body is a multipart form
with each `.gocdnext/*.yaml` as a file part + the slug/name
metadata.

### `DELETE /api/v1/projects/{slug}`

Admin-only. Cascading delete — pipelines, runs, log_lines,
secrets, all gone.

## Runs

### `GET /api/v1/runs/{id}`

Full run detail with stages, jobs, logs.

`?logs=N` — last N log lines per job (default 200, 0 disables).
`?since=jobID:seq` — repeatable; cursor-paginated delta read for
the polling client.

### `POST /api/v1/runs/{id}/cancel`

Cancel an in-flight run. 204 on success.

### `POST /api/v1/runs/{id}/rerun`

Rerun the run (same materials snapshot).

### `POST /api/v1/runs/{id}/jobs/{jobID}/rerun`

Single-job rerun.

### `POST /api/v1/runs/{id}/jobs/{jobID}/approve`

Body: `{"decision": "approve" | "reject", "comment": "..."}`.

### `GET /api/v1/runs/{id}/logs/stream`

Server-Sent Events of log lines. Format:

```
event: log
id: 12345
data: {"job_id":"...","seq":12,"stream":"stdout","at":"...","text":"..."}

event: log
id: 12346
data: {...}
```

The `Last-Event-ID` header resumes from a cursor; the platform's
SSE handler honors it.

### `GET /api/v1/runs/{id}/tests`

Aggregated JUnit results for the run.

```json
{
  "summaries": [{"job_run_id": "...", "passed": 12, "failed": 0, ...}],
  "cases": [{"name": "...", "status": "passed", ...}]
}
```

### `GET /api/v1/runs/{id}/artifacts`

```json
[{
  "id": "...",
  "job_run_id": "...",
  "job_name": "compile",
  "path": "dist/app",
  "size_bytes": 12345678,
  "status": "ready",
  "content_sha256": "...",
  "download_url": "...",
  "created_at": "..."
}]
```

`download_url` is a signed URL with a short TTL (~5 minutes).
Re-fetch the list to refresh.

## Pipelines

### `GET /api/v1/pipelines/{id}`

One pipeline's metadata + recent runs.

### `POST /api/v1/pipelines/{id}/run`

Manually trigger.

```json
{ "branch": "main", "variables": {"key": "value"} }
```

Returns the new run's id + counter.

## Plugins

### `GET /api/v1/plugins`

The catalog the server loaded on boot — manifests merged from the
baked plugins/ + extraPlugins ConfigMap.

```json
[{
  "name": "go",
  "category": "build",
  "description": "...",
  "inputs": {...},
  "examples": [...]
}]
```

The `/plugins` page in the dashboard renders this.

## Secrets

### `GET /api/v1/projects/{slug}/secrets`

Names only — values are never returned.

### `POST /api/v1/projects/{slug}/secrets`

```json
{ "name": "GHCR_TOKEN", "value": "..." }
```

### `DELETE /api/v1/projects/{slug}/secrets/{name}`

### `GET /api/v1/secrets` (admin) / `POST` / `DELETE`

Same shape, global scope.

## Settings

### `GET /api/v1/projects/{slug}/log-archive` / `PUT`

Per-project log archive override.

```json
{ "enabled": null }   // PUT body — null = inherit global
```

### `PUT /api/v1/projects/{slug}/poll-interval`

```json
{ "interval": "5m" }   // empty string disables polling
```

### `GET /api/v1/projects/{slug}/notifications` / `PUT`

Project-level notifications config.

## Admin

### `GET /api/v1/admin/users` / `PUT /{id}/role`

User management.

### `GET /api/v1/admin/groups` / `POST` / `PUT` / `DELETE`

Approver groups.

### `GET /api/v1/admin/audit`

Audit log query.

`?actor=alice@example.com&action=approval.approve&since=2026-04-01`

### `GET /api/v1/admin/retention`

Sweeper stats — last run, totals, current quotas.

### `GET /api/v1/admin/agents` / `POST` / `DELETE`

Agent provisioning. Returns the bootstrap token on POST.

### `GET /api/v1/admin/runner-profiles` / `POST` / `PUT` / `DELETE`

Runner profile CRUD.

## Webhooks

### `POST /api/v1/webhook/github`

GitHub push / PR / tag events. HMAC-validated against the SCM
source's webhook secret.

### `POST /api/v1/webhook/gitlab`

GitLab equivalent.

### `POST /api/v1/webhook/bitbucket`

Bitbucket equivalent.

The legacy `/webhook?token=...` endpoint is still wired but
deprecated — prefer the per-provider endpoints above.

## Authentication endpoints

### `GET /api/v1/auth/oauth/{provider}/start`

Redirects to the IdP for the OAuth dance. `{provider}` is one of
`github`, `google`, `keycloak`, `oidc`.

### `GET /api/v1/auth/oauth/{provider}/callback`

Where the IdP redirects back. Sets the session cookie, redirects
to `/`.

### `POST /api/v1/auth/logout`

Drops the session cookie. 204.

## Health

### `GET /healthz`

Liveness — 200 when the server is up.

### `GET /readyz`

Readiness — 200 when migrations have completed and the database
is reachable. Used by the Helm chart's `readinessProbe`.

## Metrics

### `GET /metrics`

Prometheus exposition. Server emits:

| Metric | Type | Labels |
|---|---|---|
| `gocdnext_jobs_scheduled_total` | counter | pipeline, project |
| `gocdnext_jobs_running` | gauge | (none) |
| `gocdnext_job_duration_seconds` | histogram | pipeline, project, status |
| `gocdnext_queue_depth` | gauge | stage |
| `gocdnext_grpc_server_handled_total` | counter | grpc_method, code |
| `gocdnext_log_archive_jobs_total` | counter | result |
| `gocdnext_retention_dropped_log_partitions_total` | counter | (none) |

Plus the standard Go runtime / process metrics.
