---
title: Observability (Prometheus + structured logs)
description: Wire metrics and structured logs from the gocdnext control plane into your monitoring stack.
---

gocdnext emits Prometheus metrics out of the box and writes
structured JSON logs via `slog`. Wiring them into your stack is
one scrape config (or one Helm flag if you run kube-prometheus-stack).
OpenTelemetry trace export is on the [roadmap](#opentelemetry-traces-roadmap)
but not yet wired into the binary.

## Prometheus

### What's exposed

`/metrics` on the HTTP listener (default `:8153`). The Helm
chart's service exposes this on the `http` port.

```
# HELP gocdnext_jobs_scheduled_total Total jobs the scheduler dispatched.
# TYPE gocdnext_jobs_scheduled_total counter
gocdnext_jobs_scheduled_total{pipeline="<uuid>",project="<uuid>"} 142

# HELP gocdnext_jobs_running Jobs currently in flight on this replica.
# TYPE gocdnext_jobs_running gauge
gocdnext_jobs_running 3

# HELP gocdnext_job_duration_seconds Wall-clock job duration by status.
# TYPE gocdnext_job_duration_seconds histogram
gocdnext_job_duration_seconds_bucket{status="success",le="10"} 41
…

# HELP gocdnext_queue_depth Jobs/runs in non-terminal status.
# TYPE gocdnext_queue_depth gauge
gocdnext_queue_depth{stage_status="queued"} 0
gocdnext_queue_depth{stage_status="pending"} 2

# HELP gocdnext_agents_online Agents with an active session on this replica.
# TYPE gocdnext_agents_online gauge
gocdnext_agents_online 4

# HELP gocdnext_log_archive_jobs_total Cold-archive job outcomes by result.
# TYPE gocdnext_log_archive_jobs_total counter
gocdnext_log_archive_jobs_total{result="success"} 18
gocdnext_log_archive_jobs_total{result="skipped"} 3

# HELP gocdnext_retention_dropped_log_partitions_total log_lines partitions dropped by retention sweeper.
# TYPE gocdnext_retention_dropped_log_partitions_total counter
gocdnext_retention_dropped_log_partitions_total 6

# HELP gocdnext_webhook_deliveries_total Inbound webhook deliveries by provider and outcome.
# TYPE gocdnext_webhook_deliveries_total counter
gocdnext_webhook_deliveries_total{provider="github",outcome="accepted"} 412
```

Plus the standard Go runtime metrics (`go_*`, `process_*`).

### Scrape config

```yaml title="prometheus/scrape-configs.yaml"
- job_name: gocdnext
  metrics_path: /metrics
  scrape_interval: 30s
  kubernetes_sd_configs:
    - role: service
      namespaces: { names: [gocdnext] }
  relabel_configs:
    - source_labels: [__meta_kubernetes_service_name]
      action: keep
      regex: gocdnext-server
    - source_labels: [__meta_kubernetes_service_port_name]
      action: keep
      regex: http
```

Or, if you run kube-prometheus-stack, flip the chart's
`server.serviceMonitor.enabled` flag and Helm will render the
`ServiceMonitor` for you:

```yaml title="custom-values.yaml"
server:
  serviceMonitor:
    enabled: true
    interval: 30s
    # Match the release: label your Prometheus instance selects on.
    labels:
      release: kube-prometheus-stack
```

### Useful alerts

```yaml
- alert: GocdnextHighQueueDepth
  expr: sum(gocdnext_queue_depth{stage_status="queued"}) > 10
  for: 5m
  annotations:
    summary: "Run queue stuck above 10 for 5+ minutes"

- alert: GocdnextJobFailureSpike
  expr: |
    sum(rate(gocdnext_job_duration_seconds_count{status="failed"}[10m]))
      / sum(rate(gocdnext_job_duration_seconds_count[10m])) > 0.3
  for: 15m
  annotations:
    summary: "30%+ of jobs failed in the last 15 minutes"

- alert: GocdnextNoAgents
  expr: sum(gocdnext_agents_online) == 0
  for: 2m
  annotations:
    summary: "No agents online — runs are queueing"

- alert: GocdnextLogArchiveFailing
  expr: |
    increase(gocdnext_log_archive_jobs_total{result="failed"}[1h]) > 5
  for: 15m
  annotations:
    summary: "Cold archive failed 5+ times in the last hour"
```

## OpenTelemetry traces (roadmap)

OTel trace export is **not yet wired** in `0.2.0`. The platform
already stamps `trace_id` / `span_id` slots in its slog handler so
that switching on tracing later doesn't require touching the call
sites — but the OTLP exporter is not initialised. Track progress
on [#otel-traces](https://github.com/klinux/gocdnext/issues?q=otel)
or wait for the release notes to mention it.

If you need request-flow visibility today, the structured logs
below carry `run_id`, `job_id`, `agent_id`, `pipeline` — those
correlate the same flows traces would, just without the waterfall
view.

## Logs

The platform emits structured JSON logs to stdout via `slog`:

```json
{
  "time": "2026-04-28T13:00:00Z",
  "level": "INFO",
  "msg": "agent job result",
  "run_id": "...",
  "job_id": "...",
  "job_name": "compile",
  "status": "success",
  "exit_code": 0,
  "trace_id": "...",
  "span_id": "..."
}
```

`trace_id` + `span_id` are stamped automatically when OTel is
configured — your log backend can correlate logs with traces.

### Field consistency

Every relevant span/log carries:

- `pipeline` (string, slug)
- `job` (string, name)
- `agent_id` (UUID)
- `run_id` (UUID)
- `trace_id` / `span_id` (when OTel is on)

Search across logs/traces with these labels and you get the full
picture of any run.

## Dashboards

A starter Grafana dashboard ships in
[`docs/grafana/gocdnext.json`](https://github.com/klinux/gocdnext/blob/main/docs/grafana/gocdnext.json).
It covers:

- Jobs in flight + agents online + queue stat tiles
- Dispatch rate by pipeline
- Completion rate by outcome (stacked)
- p50 / p95 / p99 job duration
- Webhook deliveries (provider × outcome)
- Log archive outcomes
- Daily partition drops + server RSS

Import via *Dashboards → New → Import* and paste the JSON; pick
your Prometheus datasource on the variables panel and you're done.

## Common pitfalls

- **Cardinality blowup**: don't add `commit_sha` as a label on
  metrics. Every commit becomes a unique time series, Prometheus
  storage explodes. The platform's built-in series keep cardinality
  bounded (no commit_sha, no per-pipeline labels on histograms) —
  be careful when adding your own.
- **Per-replica gauges**: `gocdnext_jobs_running` and
  `gocdnext_agents_online` are process-local. Use `sum()` across
  replicas for the cluster total, not `max()`.
- **`/readyz` vs `/healthz`**: wire `/readyz` to the readiness
  probe (it pings the DB, returns 503 when Postgres is down) and
  `/healthz` to the liveness probe (always 200, just proves the
  process is alive). Wiring them backwards traps a starting replica
  in a CrashLoopBackoff.
