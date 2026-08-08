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

	// QueueDepth tracks non-terminal backlog by stage_status, refreshed each
	// scheduler tick (scheduler_lifecycle.refreshQueueDepth):
	//   queued       — runs in status='queued' (run-level).
	//   pending      — queued+running job_runs (all stages).
	//   dispatchable — queued job_runs ready for an agent NOW (active stage,
	//                  unassigned, not an approval gate). This is the
	//                  autoscaling signal (#185): a KEDA/HPA scaler targets
	//                  `dispatchable`, not the coarser queued/pending. In a
	//                  multi-replica server, take max()/avg() across instances.
	QueueDepth = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gocdnext_queue_depth",
			Help: "Non-terminal backlog by stage_status: queued (runs), pending (queued+running jobs), dispatchable (queued jobs ready for an agent — the autoscaling signal).",
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

	// LogLinesDropped counts job log lines the server-side batcher
	// discarded BEFORE they reached the DB, by reason:
	//   - backpressure: the per-stream buffer's line cap was full
	//     (channel-full, 50ms wedge expired) — producer outran the
	//     flush→DB path.
	//   - bytes_full: the buffer's retained-BYTES cap was hit; a few
	//     huge lines (scanner cap is 1MB/line) can blow the heap even
	//     with channel slots free, so bytes are gated separately.
	//   - snapshot_stale: the row was reclaimed/redispatched (a newer
	//     attempt owns it) so that group was dropped.
	//   - flush_failed: the DB write errored (non-stale).
	// The agent has a mirror (gocdnext_agent_log_lines_dropped_total);
	// the two together bound "why is a job's log tail missing?".
	LogLinesDropped = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gocdnext_log_lines_dropped_total",
			Help: "Job log lines the server batcher dropped before persistence, by reason.",
		},
		[]string{"reason"},
	)

	// LogBatcherSessionDiscarded counts log lines intentionally dropped
	// when a session was superseded (Discard mode). NOT a health signal
	// like LogLinesDropped — kept a separate series so a deliberate
	// discard of a reclaimed attempt's stale tail never masquerades as
	// backpressure/data-loss on the drop metric.
	LogBatcherSessionDiscarded = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "gocdnext_log_batcher_session_discarded_total",
		Help: "Log lines the batcher dropped because the session was superseded (intentional).",
	})

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

	// RunsSuperseded counts runs canceled by latest-wins supersede (#97),
	// incremented once per superseded run when its effects fire. A rising rate is
	// healthy churn (a busy lane getting frequent newer revisions), not an error.
	// Labelless on purpose — keep supersede metrics low-cardinality (no run id /
	// branch / env).
	RunsSuperseded = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "gocdnext_runs_superseded_total",
		Help: "Runs canceled by latest-wins supersede.",
	})

	// ApprovalsExpired counts approval gates the expirer canceled because nobody
	// decided them inside their window. This is the abandonment signal: a rising
	// rate means teams are opening gates they never come back to, and the number
	// is the whole reason to measure instead of guess. Labelless to keep
	// cardinality flat (no pipeline / env / approver identity).
	ApprovalsExpired = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "gocdnext_approvals_expired_total",
		Help: "Approval gates canceled after their wait window elapsed with no decision.",
	})

	// SupersedeBackstopErrors counts dispatch-guard failures that fail CLOSED (#97):
	// the deploy is NOT dispatched and the job is left for retry. Non-zero means the
	// supersede backstop hit a DB/guard error — worth alerting on.
	SupersedeBackstopErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "gocdnext_supersede_backstop_errors_total",
		Help: "Supersede dispatch-guard errors (fail-closed; deploy not dispatched).",
	})

	// SupersedeLockBusy counts deploy dispatch attempts that couldn't take the
	// lane-env advisory lock and left the job queued for the next tick (#97). A
	// steady low rate is normal contention; a spike suggests a hot lane-env.
	SupersedeLockBusy = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "gocdnext_supersede_lock_busy_total",
		Help: "Deploy dispatches deferred because the lane-env supersede lock was held.",
	})

	// JobsReclaimed counts per-job reclaim outcomes from the reaper
	// (reason=stale) and the register-fence (reason=register_fence).
	// outcome ∈ requeued | failed_max | skipped | error. `skipped` is
	// race-normal (a snapshot-CAS no-op, amplified across replicas) —
	// don't alert on it.
	JobsReclaimed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gocdnext_jobs_reclaimed_total",
		Help: "Stale/abandoned job reclaims by reason and per-job outcome.",
	}, []string{"reason", "outcome"})

	// JobReclaimSweeps counts reclaim SWEEP calls — once per
	// ReclaimStaleJobs / ReclaimAgentJobs invocation, including empty
	// sweeps — so a top-level store error that returns before any
	// per-job result is still visible. outcome ∈ success | error.
	JobReclaimSweeps = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gocdnext_job_reclaim_sweeps_total",
		Help: "Reclaim sweeps by reason and outcome (success|error).",
	}, []string{"reason", "outcome"})

	// JobResultValidationFailed counts server-side integrity downgrades of an
	// agent-reported success (#207): the artifacts didn't reconcile, or the
	// declared outputs failed validation. It preserves the integrity signal when a
	// concurrent cancel makes the row land 'canceled' (the failure would otherwise
	// be invisible). kind is a FIXED label set — never the error message.
	JobResultValidationFailed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gocdnext_job_result_validation_failed_total",
		Help: "Server-side downgrades of a reported success by integrity kind (artifacts|outputs).",
	}, []string{"kind"})

	// JobsDisrupted counts jobs the agent reported DISRUPTED (task pod
	// preempted/evicted/node-reclaimed) by the server's verdict:
	//   - requeued: plain job under the cap → re-dispatched (new attempt).
	//   - failed_capped: the retry cap was already reached → terminal fail.
	//   - failed_unsafe_target: a deploy/environment job — never auto-retried
	//     (a partially-applied mutation must not re-run) → terminal fail.
	//   - canceled: an operator cancel won the race → terminal 'canceled'.
	// A rising `requeued` rate is spot-preemption churn; `failed_capped`
	// climbing means the cap is too low for the preemption rate. Separate
	// outcomes so "not retried for safety" is never masked as "hit the cap".
	JobsDisrupted = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gocdnext_jobs_disrupted_total",
		Help: "Jobs the agent reported DISRUPTED (pod preemption/eviction), by server outcome.",
	}, []string{"outcome"})

	// AgentDrain counts graceful-drain outcomes observed server-side at
	// stream close: clean (no in-flight jobs left) vs abandoned (jobs
	// still running → requeued by the reaper). Only draining sessions
	// emit; a crash/network disconnect does not.
	AgentDrain = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gocdnext_agent_drain_total",
		Help: "Agent graceful-drain outcomes at stream close (clean|abandoned).",
	}, []string{"outcome"})

	// AgentDrainDuration is the wall-clock from the first Draining frame
	// to stream close, split by outcome so clean/abandoned distributions
	// don't mix. Buckets span the near-zero clean case to the agent's
	// termination grace (~400s).
	AgentDrainDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gocdnext_agent_drain_duration_seconds",
		Help:    "Seconds from the Draining signal to stream close, by outcome.",
		Buckets: []float64{0.1, 0.5, 1, 5, 10, 30, 60, 120, 300, 420, 600},
	}, []string{"outcome"})

	// gRPC server metrics (#191). grpc_method is the full method string
	// (/gocdnext.v1.AgentService/<RPC>), code is status.Code(err).String().
	// Both bounded (6 methods, ~17 codes). Emitted by the hand-rolled
	// interceptors in grpcsrv (a per-message counter on the Connect
	// firehose was deliberately avoided).
	GRPCServerStarted = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gocdnext_grpc_server_started_total",
		Help: "gRPC requests started, by method.",
	}, []string{"grpc_method"})

	GRPCServerHandled = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gocdnext_grpc_server_handled_total",
		Help: "gRPC requests completed, by method and status code.",
	}, []string{"grpc_method", "code"})

	// GRPCServerHandling is UNARY-only: the long-lived Connect stream's
	// handling time is the whole session, which would pollute a latency
	// histogram, so the stream interceptor does not observe it.
	GRPCServerHandling = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gocdnext_grpc_server_handling_seconds",
		Help:    "Handling latency of unary gRPC methods (the Connect stream is excluded).",
		Buckets: prometheus.DefBuckets,
	}, []string{"grpc_method"})

	// GateFreezeAnnotationErrors counts degraded computations of a gate's
	// freeze-hold state (#227), on either surface that renders it: the run-detail
	// page or the project-detail flow/list. A run snapshot that won't decode
	// (kind=decode) or a freeze lookup that errored (kind=lookup). The result
	// fails safe (no badge), so this is the ONLY signal that a persistent failure
	// is leaving Approve enabled — alert on a rising rate. surface + kind are
	// FIXED label sets, never the error message.
	GateFreezeAnnotationErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gocdnext_gate_freeze_annotation_errors_total",
		Help: "Degraded gate freeze-hold annotations by surface (run_detail|project_detail) and kind (decode|lookup).",
	}, []string{"surface", "kind"})

	// PRHeadResolution measures PR-head config resolution (#223): the wall-clock
	// to fetch + parse + validate the head `.gocdnext/` and build the plan,
	// labelled by outcome. Only the GitHub same-repo opted-in path with repo
	// pipelines to resolve emits — the base flow never resolves. outcome is a
	// FIXED, bounded set (ok|fetch_error|invalid) — never the error message, and
	// no repo/branch/pipeline label (keep cardinality flat).
	PRHeadResolution = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gocdnext_pr_head_resolution_seconds",
		Help:    "PR-head config resolution duration by outcome (ok|fetch_error|invalid).",
		Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 30},
	}, []string{"outcome"})
)

func init() {
	Registry.MustRegister(
		JobsScheduled,
		JobsRunning,
		JobDurationSeconds,
		QueueDepth,
		AgentsOnline,
		LogArchiveJobs,
		LogLinesDropped,
		LogBatcherSessionDiscarded,
		RetentionDroppedLogPartitions,
		WebhookDeliveries,
		RunsSuperseded,
		ApprovalsExpired,
		SupersedeBackstopErrors,
		SupersedeLockBusy,
		JobsReclaimed,
		JobReclaimSweeps,
		JobResultValidationFailed,
		JobsDisrupted,
		AgentDrain,
		AgentDrainDuration,
		GRPCServerStarted,
		GRPCServerHandled,
		GRPCServerHandling,
		PRHeadResolution,
		GateFreezeAnnotationErrors,
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
	// domain.StatusCanceled is "canceled" (one l); the old two-l "cancelled" here
	// meant every canceled job bucketed as "unknown" (#207). GitHub Checks keep
	// their own two-l "cancelled" conclusion — that's the GitHub API spelling.
	case "success", "failed", "canceled", "skipped":
		return status
	default:
		return "unknown"
	}
}
