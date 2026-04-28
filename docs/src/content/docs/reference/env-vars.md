---
title: Environment variables
description: Every GOCDNEXT_* env var the server reads on boot, organised by surface.
---

This page catalogs every `GOCDNEXT_*` env var consumed by the
server's [config loader](https://github.com/klinux/gocdnext/blob/main/server/internal/config/config.go).
The Helm chart exposes most of these via the `values.yaml` blocks
that map 1:1 — see [Helm install](/gocdnext/docs/install/helm/)
for the value-side names.

## Core

| Var | Default | Notes |
|---|---|---|
| `GOCDNEXT_DATABASE_URL` | (required) | Postgres DSN. Wired via Helm secret. |
| `GOCDNEXT_HTTP_ADDR` | `:8153` | HTTP listen address |
| `GOCDNEXT_GRPC_ADDR` | `:8154` | gRPC listen address (agent stream) |
| `GOCDNEXT_PUBLIC_BASE` | empty | Externally-reachable base URL. Required for OAuth callbacks + webhook URLs. |
| `GOCDNEXT_LOG_LEVEL` | `info` | `debug \| info \| warn \| error` |
| `GOCDNEXT_WEBHOOK_TOKEN` | empty | Pre-shared token for the legacy `/webhook?token=…` endpoint |
| `GOCDNEXT_WEBHOOK_PUBLIC_URL` | empty | Override for the URL handed to GitHub when registering a repo webhook. Defaults to `GOCDNEXT_PUBLIC_BASE`. |

## Secrets backend

| Var | Default | Notes |
|---|---|---|
| `GOCDNEXT_SECRET_KEY` | (required) | AES-256-GCM key (64 hex chars) for project-secret encryption. |
| `GOCDNEXT_SECRET_BACKEND` | `db` | `db \| kubernetes` |
| `GOCDNEXT_SECRET_K8S_NAMESPACE` | release ns | Namespace for K8s-backed secrets |
| `GOCDNEXT_SECRET_K8S_NAME_TEMPLATE` | `gocdnext-secrets-{slug}` | Secret name template; `{slug}` = project slug |

## Artifacts

| Var | Default | Notes |
|---|---|---|
| `GOCDNEXT_ARTIFACTS_BACKEND` | `filesystem` | `filesystem \| s3 \| gcs` |
| `GOCDNEXT_ARTIFACTS_FS_ROOT` | `/var/lib/gocdnext/artifacts` | Filesystem backend storage root |
| `GOCDNEXT_ARTIFACTS_PUBLIC_BASE` | = public_base | Base URL artefacts are downloaded from |
| `GOCDNEXT_ARTIFACTS_SIGN_KEY` | (required) | HMAC key for signed download URLs |
| `GOCDNEXT_ARTIFACTS_KEEP_LAST` | `30` | Keep N most recent runs per pipeline; 0 disables |
| `GOCDNEXT_ARTIFACTS_PROJECT_QUOTA_BYTES` | `107374182400` | Per-project soft cap (100 GiB). 0 disables. |
| `GOCDNEXT_ARTIFACTS_GLOBAL_QUOTA_BYTES` | `0` | Global hard cap. 0 = disabled. |
| `GOCDNEXT_ARTIFACTS_MAX_BODY_MB` | `2048` | Per-request body cap on uploads |
| `GOCDNEXT_ARTIFACTS_S3_BUCKET` | empty | S3 bucket name (when backend=s3) |
| `GOCDNEXT_ARTIFACTS_S3_REGION` | `us-east-1` | |
| `GOCDNEXT_ARTIFACTS_S3_ENDPOINT` | empty | Custom S3-compatible endpoint (MinIO, R2, etc.) |
| `GOCDNEXT_ARTIFACTS_S3_USE_PATH_STYLE` | `false` | `true` for MinIO and most S3-compatibles |
| `GOCDNEXT_ARTIFACTS_S3_ENSURE_BUCKET` | `false` | Auto-create the bucket on boot |
| `GOCDNEXT_ARTIFACTS_S3_ACCESS_KEY` | empty | Plumbed via Helm secret |
| `GOCDNEXT_ARTIFACTS_S3_SECRET_KEY` | empty | Plumbed via Helm secret |
| `GOCDNEXT_ARTIFACTS_GCS_BUCKET` | empty | GCS bucket (when backend=gcs) |
| `GOCDNEXT_ARTIFACTS_GCS_PROJECT_ID` | empty | Required for ensure_bucket |
| `GOCDNEXT_ARTIFACTS_GCS_ENSURE_BUCKET` | `false` | |
| `GOCDNEXT_ARTIFACTS_GCS_CREDENTIALS_FILE` | empty | Service-account JSON path |
| `GOCDNEXT_ARTIFACTS_GCS_CREDENTIALS_JSON` | empty | Service-account JSON content (alternative to file) |

## Cache (per-job content cache, not log archive)

| Var | Default | Notes |
|---|---|---|
| `GOCDNEXT_CACHE_TTL_DAYS` | `30` | Inactivity window before eviction |
| `GOCDNEXT_CACHE_PROJECT_QUOTA_BYTES` | `0` | Per-project cap (disabled by default) |
| `GOCDNEXT_CACHE_GLOBAL_QUOTA_BYTES` | `0` | Global cap (disabled by default) |

## Logs

| Var | Default | Notes |
|---|---|---|
| `GOCDNEXT_LOG_RETENTION` | empty | Drop log_lines partitions older than this duration. Empty = no drop. Format: Go duration (`720h`, `30d`). |
| `GOCDNEXT_LOG_MONTHS_AHEAD` | `3` | Months of partitions stocked ahead of "now" |
| `GOCDNEXT_LOG_ARCHIVE` | `auto` | `auto \| on \| off`. `auto` = on iff artefact backend wired. |
| `GOCDNEXT_LOG_ARCHIVE_CACHE_BYTES` | `268435456` | LRU cache for decoded archives. 0 = disabled. (256 MiB) |

## Authentication

| Var | Default | Notes |
|---|---|---|
| `GOCDNEXT_AUTH_ENABLED` | `false` | Master switch. `false` keeps every route open. |
| `GOCDNEXT_AUTH_ADMIN_EMAILS` | empty | Comma-separated list. First-login users on this list become admin. |
| `GOCDNEXT_AUTH_ALLOWED_DOMAINS` | empty | Comma-separated. Logins from other domains are rejected. |
| `GOCDNEXT_AUTH_GITHUB_CLIENT_ID` | empty | OAuth app client id |
| `GOCDNEXT_AUTH_GITHUB_CLIENT_SECRET` | empty | Wired via Helm secret |
| `GOCDNEXT_AUTH_GITHUB_API_BASE` | empty | Override for GitHub Enterprise (`https://github.example.com/api/v3`) |
| `GOCDNEXT_AUTH_GOOGLE_CLIENT_ID` | empty | |
| `GOCDNEXT_AUTH_GOOGLE_CLIENT_SECRET` | empty | |
| `GOCDNEXT_AUTH_GOOGLE_ISSUER` | `https://accounts.google.com` | OIDC issuer URL |
| `GOCDNEXT_AUTH_KEYCLOAK_CLIENT_ID` | empty | |
| `GOCDNEXT_AUTH_KEYCLOAK_CLIENT_SECRET` | empty | |
| `GOCDNEXT_AUTH_KEYCLOAK_ISSUER` | empty | `https://kc.example.com/realms/<name>` |
| `GOCDNEXT_AUTH_OIDC_CLIENT_ID` | empty | Generic OIDC fallback |
| `GOCDNEXT_AUTH_OIDC_CLIENT_SECRET` | empty | |
| `GOCDNEXT_AUTH_OIDC_ISSUER` | empty | `https://idp.example.com` |
| `GOCDNEXT_AUTH_OIDC_NAME` | empty | Display name on the login button |

## GitHub App (for higher rate limits + Checks API)

| Var | Default | Notes |
|---|---|---|
| `GOCDNEXT_GITHUB_APP_ID` | empty | App ID |
| `GOCDNEXT_GITHUB_APP_PRIVATE_KEY` | empty | Inline PEM (alternative to file) |
| `GOCDNEXT_GITHUB_APP_PRIVATE_KEY_FILE` | empty | PEM file path; mounted from a Helm secret |
| `GOCDNEXT_GITHUB_APP_API_BASE` | `https://api.github.com` | Override for GHE |

## Plugin catalog

| Var | Default | Notes |
|---|---|---|
| `GOCDNEXT_PLUGIN_CATALOG_DIR` | `/etc/gocdnext/plugins` (set in Dockerfile) | Colon-separated path list. The chart appends `/etc/gocdnext/extra-plugins` when `extraPlugins:` is non-empty. |

## Runner profiles

| Var | Default | Notes |
|---|---|---|
| `GOCDNEXT_RUNNER_PROFILES_FILE` | empty | YAML the chart mounts via ConfigMap; server upserts entries on boot |

## Agent (separate binary, not server)

The agent's binary reads its own env. Most operators set these via
the chart's `agent:` values block.

| Var | Default | Notes |
|---|---|---|
| `GOCDNEXT_SERVER_ADDR` | (required) | gRPC endpoint (`server.example.com:8154`) |
| `GOCDNEXT_AGENT_NAME` | (required) | Unique per agent — must match a registered name on the server |
| `GOCDNEXT_AGENT_TOKEN` | (required) | Pre-provisioned auth token |
| `GOCDNEXT_AGENT_TAGS` | empty | Comma-separated. Used to route jobs (`agent.tags:` in YAML). |
| `GOCDNEXT_AGENT_CAPACITY` | `2` | Max concurrent jobs |
| `GOCDNEXT_AGENT_ENGINE` | `docker` | `docker \| kubernetes \| shell` |
| `GOCDNEXT_DOCKER_SOCKET` | `/var/run/docker.sock` | (engine=docker) |
| `GOCDNEXT_DOCKER_PULL_POLICY` | empty | `always \| missing \| never`. Empty = docker default (`missing`). |
| `GOCDNEXT_DOCKER_STRICT` | `false` | Reject `image:` references not in the plugin catalog |
| `GOCDNEXT_DOCKER_EXTRA_ARGS` | empty | Extra args appended to every `docker run` (`--init`, etc.) |
| `GOCDNEXT_K8S_NAMESPACE` | empty | (engine=kubernetes) Namespace where job Pods spawn |
| `GOCDNEXT_K8S_KUBECONFIG` | empty | Path to kubeconfig; empty = in-cluster service account |
| `GOCDNEXT_K8S_WORKSPACE_PVC` | empty | PVC name; empty = emptyDir |

## Observability

| Var | Default | Notes |
|---|---|---|
| `GOCDNEXT_OTEL_EXPORTER_OTLP_ENDPOINT` | empty | OTLP traces endpoint. Empty = disabled. |
| `GOCDNEXT_PROMETHEUS_ENABLED` | `true` | Expose `/metrics` on the HTTP listener |

The OTel SDK reads the standard `OTEL_*` vars too — anything those
control is also controllable via plain `OTEL_RESOURCE_ATTRIBUTES`,
`OTEL_SERVICE_NAME`, etc.
