// Package metrics owns the Prometheus exposition surface for the AGENT. It
// mirrors the server's metrics package idiom (a single process-wide registry,
// package-level instruments, one Handler) so the two read the same way.
//
// What lives here is only what the agent uniquely knows: log-line loss under
// backpressure, and where a job spent its wall-clock (prep/task/post_job). The
// authoritative running/capacity-per-agent gauges live SERVER-side, where the
// numbers already are — the agent does not need to be scrapable for those.
//
// Conventions match the server: `gocdnext_agent_<noun>_<verb>_<unit>`, labels
// kept to a bounded closed set (never job/run/commit ids), histograms on an
// explicit range rather than the client default so multi-minute prep/clone
// times are not all lumped into the top bucket.
package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Registry is the agent's single collection point, exposed for tests that want
// to scrape a fresh registry rather than the process-wide one.
var Registry = prometheus.NewRegistry()

var (
	// LogLinesDropped counts log lines the agent dropped because the outbound
	// stream buffer was full. Dropping is deliberate (blocking the producer
	// would deadlock the job — the JobResult could never be sent), so this is
	// the only signal that a truncated build log is loss, not silence. Labelless
	// on purpose: the interesting cut is per-agent, which the scrape target
	// (pod/instance) already provides.
	LogLinesDropped = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "gocdnext_agent_log_lines_dropped_total",
		Help: "Log lines dropped because the outbound stream buffer was full.",
	})

	// JobPhaseSeconds is the wall-clock a job spent in each coarse phase. The
	// `phase` label is a CLOSED set (prep|task|post_job) — deliberately not the
	// free-form human phase markers, which would be unbounded. `post_job` is a
	// single aggregate (artifacts + caches) because isolated mode cannot split
	// them without instrumenting inside PostJob.
	JobPhaseSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "gocdnext_agent_job_duration_seconds",
			Help:    "Wall-clock a job spent in each phase (prep, task, post_job).",
			Buckets: prometheus.ExponentialBucketsRange(0.005, 600, 16),
		},
		[]string{"phase"},
	)
)

func init() {
	Registry.MustRegister(
		LogLinesDropped,
		JobPhaseSeconds,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
}

// Handler exposes the registry — wire it on the agent's metrics listener.
func Handler() http.Handler {
	return promhttp.HandlerFor(Registry, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
		Registry:          Registry,
	})
}

// ObserveJobPhase records a phase duration, collapsing any unexpected phase to
// "unknown" so a future phase name can never blow up cardinality.
func ObserveJobPhase(phase string, d time.Duration) {
	JobPhaseSeconds.WithLabelValues(phaseLabel(phase)).Observe(d.Seconds())
}

func phaseLabel(phase string) string {
	switch phase {
	case "prep", "task", "post_job":
		return phase
	default:
		return "unknown"
	}
}
