package grpcsrv

import "github.com/prometheus/client_golang/prometheus"

// Per-agent gauges, emitted at scrape time from the live SessionStore. Raw
// running and capacity (not a pre-divided ratio) so PromQL can alert on the
// ratio. Labeled by agent UUID — bounded by fleet size, the same low-cardinality
// tier as the pipeline/project UUID labels elsewhere.
var (
	agentJobsRunningDesc = prometheus.NewDesc(
		"gocdnext_agent_jobs_running",
		"Job assignments in flight on an agent session, from this server replica.",
		[]string{"agent"}, nil,
	)
	agentCapacityDesc = prometheus.NewDesc(
		"gocdnext_agent_capacity",
		"Configured max concurrent jobs for an agent session.",
		[]string{"agent"}, nil,
	)
)

// AgentSessionCollector emits per-agent running/capacity at scrape time from the
// authoritative in-memory SessionStore. Pull-time collection means the numbers
// are always fresh with no periodic tick, and it needs no agent to be scrapable.
//
// It is constructed once and registered once at bootstrap — NOT inside a
// per-service constructor — because the server metrics.Registry is a global and
// a second registration (a second SessionStore, notably in tests) would panic on
// duplicate registration.
type AgentSessionCollector struct {
	store *SessionStore
}

// NewAgentSessionCollector returns a collector over the given store. Register it
// exactly once (e.g. metrics.Registry.MustRegister at bootstrap).
func NewAgentSessionCollector(store *SessionStore) *AgentSessionCollector {
	return &AgentSessionCollector{store: store}
}

func (c *AgentSessionCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- agentJobsRunningDesc
	ch <- agentCapacityDesc
}

func (c *AgentSessionCollector) Collect(ch chan<- prometheus.Metric) {
	// CapacitySnapshot takes s.mu; emit to the channel OUTSIDE that lock.
	for _, s := range c.store.CapacitySnapshot() {
		agent := s.AgentID.String()
		ch <- prometheus.MustNewConstMetric(agentJobsRunningDesc, prometheus.GaugeValue, float64(s.Running), agent)
		ch <- prometheus.MustNewConstMetric(agentCapacityDesc, prometheus.GaugeValue, float64(s.Capacity), agent)
	}
}
