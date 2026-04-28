---
title: Observability (OpenTelemetry + Prometheus)
description: Wire traces, metrics, and structured logs from the gocdnext control plane into your monitoring stack.
---

gocdnext emits OpenTelemetry traces and Prometheus metrics out of
the box. The control plane and the agent both speak both surfaces;
wiring them into your stack is one env var (OTel) and one scrape
config (Prometheus).

## Prometheus

### What's exposed

`/metrics` on the HTTP listener (default `:8153`). The Helm
chart's service exposes this on the `http` port.

```
# HELP gocdnext_jobs_scheduled_total Total jobs the scheduler dispatched.
# TYPE gocdnext_jobs_scheduled_total counter
gocdnext_jobs_scheduled_total{pipeline="ci-server",project="gocdnext"} 142

# HELP gocdnext_jobs_running Jobs currently in flight.
# TYPE gocdnext_jobs_running gauge
gocdnext_jobs_running 3

# HELP gocdnext_job_duration_seconds Wall-clock job duration.
# TYPE gocdnext_job_duration_seconds histogram
gocdnext_job_duration_seconds_bucket{pipeline="ci-server",project="gocdnext",status="success",le="10"} 41
...

# HELP gocdnext_queue_depth Jobs queued per stage.
# TYPE gocdnext_queue_depth gauge
gocdnext_queue_depth{stage="lint"} 0

# HELP gocdnext_grpc_server_handled_total gRPC handlers, by method + code.
# TYPE gocdnext_grpc_server_handled_total counter
gocdnext_grpc_server_handled_total{grpc_method="Connect",code="OK"} 234
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

Or with the Helm-friendly Prometheus operator's `ServiceMonitor`:

```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: gocdnext
  namespace: gocdnext
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: gocdnext
  endpoints:
    - port: http
      path: /metrics
      interval: 30s
```

### Useful alerts

```yaml
- alert: GocdnextHighQueueDepth
  expr: gocdnext_queue_depth > 10
  for: 5m
  annotations:
    summary: "{{ $labels.stage }} stage queue stuck above 10 jobs for 5+ minutes"

- alert: GocdnextJobFailureSpike
  expr: |
    rate(gocdnext_jobs_finished_total{status="failed"}[10m])
      / rate(gocdnext_jobs_finished_total[10m]) > 0.3
  for: 15m
  annotations:
    summary: "30%+ of jobs failed in the last 15 minutes"

- alert: GocdnextAgentDisconnected
  expr: gocdnext_agents_online == 0
  for: 2m
  annotations:
    summary: "No agents are online — runs are queueing"
```

## OpenTelemetry traces

### Enable

Set the OTLP endpoint via env (Helm wires this through the
chart's `server.env:` extension):

```yaml
server:
  env:
    - name: OTEL_EXPORTER_OTLP_ENDPOINT
      value: http://tempo.observability.svc:4317
    - name: OTEL_SERVICE_NAME
      value: gocdnext-server
    - name: OTEL_RESOURCE_ATTRIBUTES
      value: deployment.environment=prod,service.version=0.2.0
```

The control plane connects on boot and starts shipping spans.
Same env vars work on the agent (set under the chart's `agent:`
block).

### What's traced

Spans named `pipeline.parse`, `run.create`, `job.dispatch`,
`agent.stream.recv`, `webhook.receive`, plus every HTTP handler
and every gRPC method. The trace tree mirrors the actual flow:
a webhook span has child spans for parse, scheduler dispatch,
and the per-job agent dispatch — letting you see exactly where
time is spent in a slow run.

### Sampling

Default is `parentbased_traceidratio` at 1.0 (every trace
sampled). For high-volume deployments, override:

```yaml
- name: OTEL_TRACES_SAMPLER
  value: parentbased_traceidratio
- name: OTEL_TRACES_SAMPLER_ARG
  value: "0.1"     # 10% of new traces
```

### Backend

Anything that speaks OTLP (gRPC on `:4317` or HTTP on `:4318`):

- **Jaeger** — first-party
- **Tempo** — Grafana stack
- **Honeycomb** / **Datadog** / **NewRelic** — managed
- **OpenTelemetry Collector** — for fan-out / filtering /
  transformation before forwarding to multiple backends

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
[`docs/grafana/gocdnext.json`](https://github.com/klinux/gocdnext/blob/main/docs/grafana/gocdnext.json)
(when it lands; the placeholder is on the roadmap). It covers:

- Jobs in flight (gauge)
- Job rate (success / failed / cancelled, stacked)
- Queue depth per stage
- p50 / p95 / p99 job duration per pipeline
- Agent count (online / total)
- Webhook delivery rate per provider

Import via *Dashboards → New → Import* and paste the JSON.

## Common pitfalls

- **OTLP without TLS in production**: the default `http://` is
  plaintext gRPC. Use `https://` or the explicit `OTEL_EXPORTER_OTLP_PROTOCOL`
  control. For in-cluster Tempo / Jaeger, plaintext on a
  ClusterIP service is usually fine.
- **Cardinality blowup**: don't add `commit_sha` as a label on
  metrics. Every commit becomes a unique time series, Prometheus
  storage explodes. The platform's metrics already keep cardinality
  bounded — be careful when adding custom labels via OTel
  attributes.
- **Trace context not propagating to plugin containers**: the
  agent doesn't inject OTel context into the jobs it runs. If
  you want plugin spans, the plugin needs to start its own trace
  with parent set from `traceparent` env (which the agent does
  inject). Most plugins don't bother; the trace tree stops at
  job dispatch.
