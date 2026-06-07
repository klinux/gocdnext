---
title: HTTP API
description: REST endpoints the dashboard, the CLI, and external integrations use to read + write platform state.
---

The control plane exposes REST API endpoints at `/api/v1/` (plus
webhook + auth routes outside the `/v1/` namespace). Every `/api/v1`
endpoint requires authentication when `auth.enabled=true` — either
a session cookie (browser flow) or a bearer token
(`Authorization: Bearer <token>`, machine flow). API tokens are
issued at `/account` (per-user) or `/admin/service-accounts`
(machine identities); they inherit the role of the issuer.

This page is a tour of the most-used endpoints. The full surface
lives in `server/cmd/gocdnext-server/main.go` (route table) and
`server/internal/api/` (handlers).

## Conventions

- All responses are JSON (`Content-Type: application/json`).
- All times are RFC 3339 with timezone (`2026-04-28T13:00:00Z`).
- IDs are UUIDs (string).
- Errors use HTTP status + a JSON body `{"error": "human-readable message"}`.
- List endpoints support `?limit=N&offset=M` and return a JSON
  envelope with `items` or a named array.

## Projects

### `GET /api/v1/projects`

List projects the caller has access to.

```bash
curl -H "Authorization: Bearer $TOKEN" \
  https://ci.example.com/api/v1/projects
```

### `GET /api/v1/projects/{slug}`

Fetch one project plus recent runs + pipelines.

### `POST /api/v1/projects/apply`

The endpoint the CLI's `apply` hits. Body is a multipart form
with each `.gocdnext/*.yaml` as a file part + the slug/name
metadata.

### `POST /api/v1/projects/{slug}/sync`

Force a re-poll of all materials on this project's pipelines.
Useful when a webhook delivery was missed.

### `DELETE /api/v1/projects/{slug}`

Admin-only. Cascading delete — pipelines, runs, log_lines,
secrets, all gone.

### `POST /api/v1/projects/{slug}/run-all`

Trigger every pipeline in the project against `HEAD` of the
default branch.

### `GET /api/v1/projects/{slug}/vsm`

Returns the Value Stream Map for the project (nodes + edges +
latest-run-per-pipeline metadata, with `has_services` flags).

## Runs

### `GET /api/v1/runs`

Global runs feed (admin/dashboard wide view).

### `GET /api/v1/runs/{id}`

Full run detail with stages, jobs, logs.

`?logs=N` — last N log lines per job (default 200, 0 disables).

### `GET /api/v1/runs/{id}/logs/stream`

Server-Sent Events of log lines. Format:

```
event: log
id: 12345
data: {"job_id":"...","seq":12,"stream":"stdout","at":"...","text":"..."}
```

The `Last-Event-ID` header resumes from a cursor; the platform's
SSE handler honors it.

### `GET /api/v1/runs/{id}/tests`

Aggregated JUnit results for the run.

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

### `GET /api/v1/runs/{id}/services`

Service-run rows for the run (service lifecycle — `starting`,
`ready`, `failed`, `stopped`).

### `POST /api/v1/runs/{id}/cancel`

Cancel an in-flight run. 204 on success.

### `POST /api/v1/runs/{id}/rerun`

Rerun the run (same materials snapshot).

### `POST /api/v1/job_runs/{id}/rerun`

Single-job rerun. The `{id}` is the `job_run` UUID (NOT the
parent run's UUID).

### `POST /api/v1/job_runs/{id}/approve`

Approve an `awaiting_approval` job. Body:

```json
{ "comment": "optional reviewer note" }
```

### `POST /api/v1/job_runs/{id}/reject`

Reject a gate. Same body shape. The run flips to `failed`.

## Pipelines

### `GET /api/v1/pipelines/{id}/yaml`

The raw `.gocdnext/<pipeline>.yaml` the project applied with.

### `POST /api/v1/pipelines/{id}/trigger`

Manually trigger a run. Cause is `manual`. Returns the new run's
id + counter.

## Plugins

### `GET /api/v1/plugins`

The catalog the server loaded on boot — manifests merged from the
baked `plugins/` + the `extraPlugins` ConfigMap.

## Secrets

### `GET /api/v1/projects/{slug}/secrets`

Names + timestamps only — values are never returned.

### `POST /api/v1/projects/{slug}/secrets`

```json
{ "name": "GHCR_TOKEN", "value": "..." }
```

### `DELETE /api/v1/projects/{slug}/secrets/{name}`

### `GET /api/v1/admin/secrets`

Global (cross-project) secrets. Admin-only. `POST` + `DELETE`
follow the same shape.

## Account

### `GET /api/v1/account/preferences` / `PUT`

Per-user dashboard preferences (stored in `user_preferences`
JSONB).

### `GET /api/v1/account/api-tokens` / `POST` / `DELETE`

Per-user API token management. `POST` returns the plaintext token
exactly once.

## Settings

### `GET /api/v1/projects/{slug}/log-archive` / `PUT`

Per-project log archive override (null = inherit global).

### `PUT /api/v1/projects/{slug}/poll-interval`

```json
{ "interval": "5m" }   // empty string disables polling
```

### `GET /api/v1/projects/{slug}/notifications` / `PUT`

Project-level notifications config.

### `GET /api/v1/projects/{slug}/caches` / `DELETE /{id}`

List cache entries; admin purge by id.

### `GET /api/v1/projects/{slug}/crons` / `POST` / `PUT /{id}` / `DELETE /{id}`

Project cron schedules.

## Admin

### `GET /api/v1/admin/users` / `POST` / `PUT /{id}/role`

User management.

### `GET /api/v1/admin/audit`

Audit log query.

`?actor=alice@example.com&action=approval.approve&since=2026-04-01`

### `GET /api/v1/admin/retention`

Sweeper stats — last run, totals, current quotas.

### `GET /api/v1/admin/storage` / `PUT` / `DELETE`

Storage-backend runtime config (`/settings/storage` UI surface).
`PUT` may return `X-Gocdnext-Restart-Required: true` when the
backend change needs a server bounce to take effect.

### `GET /api/v1/admin/health`

Detailed health report (DB connection, scheduler tick lag, agent
fleet status).

### `GET /api/v1/admin/webhooks` / `GET /{id}`

Inbound webhook delivery audit (history + per-delivery detail).

### `GET /api/v1/admin/integrations` / `/admin/integrations/github`

Configured SCM integrations.

### `GET /api/v1/admin/scm-credentials` / `POST`

CRUD for SCM provider credentials (GitHub App key, GitLab PAT,
…).

### `GET /api/v1/admin/auth/providers` / `POST` / `DELETE /{id}` / `POST /reload`

Auth provider CRUD. `POST /reload` forces the provider registry
to re-read after editing.

### `GET /api/v1/agents` / `GET /{id}`

Agent fleet read endpoints (no admin write today — agents are
provisioned via the chart, not the API).

## Webhooks (unauthenticated, HMAC-validated)

### `POST /api/webhooks/github`

GitHub push / PR / tag events. HMAC-validated against the SCM
source's webhook secret.

### `POST /api/webhooks/gitlab`

GitLab equivalent.

### `POST /api/webhooks/bitbucket`

Bitbucket equivalent.

Note the route prefix: `/api/webhooks/` (plural, NO `/v1`). This
is the only API namespace that lives outside `/api/v1/`.

A single push fans out to every pipeline matching the
fingerprint; the response body is `{ "runs": ["<uuid>", ...] }`.

## Authentication endpoints

Auth routes live under `/auth/*` (NOT `/api/v1/auth/`).

### `GET /auth/providers`

Returns the enabled provider list (used by the login page to
render buttons).

### `GET /auth/login/{provider}`

Redirects to the IdP for the OAuth dance. `{provider}` is
`github`, `google`, `keycloak`, `oidc`, or `local` (for
local-password login).

### `GET /auth/callback/{provider}`

Where the IdP redirects back. Sets the session cookie, redirects
to the original target.

### `POST /auth/login/local`

Local-password login.

### `POST /auth/logout`

Drops the session cookie. 204.

### `GET /api/v1/me`

The session subject — used by the dashboard to render the user
chip.

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
