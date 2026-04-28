// Package metrics owns the Prometheus exposition surface for the
// control plane. Every series is registered against a single
// process-wide registry that's exposed at `/metrics`. The
// instruments live in package-level vars so any other package
// can record a sample with one import + one call — no DI plumbing.
//
// Conventions:
//   - Series names match `gocdnext_<noun>_<verb>_<unit>` exactly,
//     so they flow through prometheus_grafana relabel rules without
//     surprises.
//   - Label cardinality is bounded by deliberately keeping
//     project/pipeline names as labels and NOT commit_sha. A new
//     pipeline adds one series; a new commit adds nothing.
//   - Histograms use Prometheus' default exponential buckets
//     covering 5ms..10s — captures both fast scheduler cycles and
//     long-running test suites.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Registry is the single collection point. Exposed so tests can
// pull a fresh registry per case via `Reset()` rather than racing
// the process-wide default.
var Registry = prometheus.NewRegistry()

// Series — exposed for the rest of the codebase to .Inc / .Set /
// .Observe against. All registered at init time so a missed init
// is a hard panic at boot, not a silent no-op at runtime.
var (
	// JobsScheduled counts every job the scheduler successfully
	// dispatched (claim succeeded + assignment placed on the
	// session queue). Lost-race retries do NOT increment.
	JobsScheduled = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gocdnext_jobs_scheduled_total",
			Help: "Total jobs the scheduler dispatched.",
		},
		[]string{"pipeline", "project"},
	)

	// JobsRunning is a process-local gauge that tracks the number
	// of active job assignments per server replica. With multiple
	// replicas, sum() across instances gives the cluster total.
	JobsRunning = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "gocdnext_jobs_running",
		Help: "Jobs currently in flight from the perspective of this server replica.",
	})

	// JobDurationSeconds is the wall-clock from dispatch to
	// terminal status. Status splits success/failed/cancelled so
	// dashboards see error-path vs happy-path latency separately.
	// Pipeline/project labels are deliberately omitted — would
	// require an extra join per observation; operators wanting
	// per-pipeline latency split via the slog "agent job result"
	// line (carries pipeline + duration_ms) or future OTel traces.
	JobDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "gocdnext_job_duration_seconds",
			Help:    "Wall-clock duration of jobs from dispatch to terminal status.",
			Buckets: prometheus.ExponentialBucketsRange(0.005, 600, 16),
		},
		[]string{"status"},
	)

	// QueueDepth tracks jobs waiting per stage type (queued,
	// awaiting_approval). Updated by the sweeper's tick from a
	// SELECT count(*) GROUP BY stage; reading is cheap.
	QueueDepth = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gocdnext_queue_depth",
			Help: "Jobs in non-terminal status, grouped by stage state.",
		},
		[]string{"stage_status"},
	)

	// AgentsOnline is a process-local count of registered agent
	// sessions. Sum across replicas for the cluster total.
	AgentsOnline = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "gocdnext_agents_online",
		Help: "Agents with an active session on this replica.",
	})

	// LogArchiveJobs counts archive attempts by terminal result —
	// success, fail, skipped (job had no logs). Useful for alerting
	// when fail rate climbs.
	LogArchiveJobs = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gocdnext_log_archive_jobs_total",
			Help: "Cold-archive job outcomes by result.",
		},
		[]string{"result"},
	)

	// RetentionDroppedLogPartitions counts how many monthly
	// partitions the sweeper has dropped. Constant-time DROP, so
	// big numbers here are healthy — they mean the partitioning
	// is doing its job.
	RetentionDroppedLogPartitions = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "gocdnext_retention_dropped_log_partitions_total",
		Help: "log_lines partitions dropped by the retention sweeper.",
	})

	// WebhookDeliveries counts incoming webhooks per provider with
	// the HTTP outcome the platform replied with. Lets dashboards
	// distinguish "we accepted it" from "HMAC mismatch" from
	// "unknown source".
	WebhookDeliveries = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gocdnext_webhook_deliveries_total",
			Help: "Inbound webhook deliveries by provider and outcome.",
		},
		[]string{"provider", "outcome"},
	)
)

func init() {
	Registry.MustRegister(
		JobsScheduled,
		JobsRunning,
		JobDurationSeconds,
		QueueDepth,
		AgentsOnline,
		LogArchiveJobs,
		RetentionDroppedLogPartitions,
		WebhookDeliveries,
		// Standard Go runtime + process metrics for free.
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
}

// Handler returns the http.Handler that exposes the registry —
// wire this on the public listener at /metrics. Uses `EnableOpenMetrics`
// so OpenMetrics-compatible scrapers (Prometheus, Grafana Cloud,
// Datadog OTel collector) get the richer encoding.
func Handler() http.Handler {
	return promhttp.HandlerFor(Registry, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
		Registry:          Registry,
	})
}

// JobStatusLabel normalises the variety of status strings the
// platform uses internally into the bounded label set the
// histogram expects. Anything unrecognised → "unknown" so a
// future status doesn't blow up cardinality.
func JobStatusLabel(status string) string {
	switch status {
	case "success", "failed", "cancelled", "skipped":
		return status
	default:
		return "unknown"
	}
}
