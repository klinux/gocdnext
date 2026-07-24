package grpcsrv

import (
	"testing"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
)

// gaugeFor gathers reg and returns the value of `name{agent=…}`, or (0,false).
func gaugeFor(t *testing.T, reg *prometheus.Registry, name, agent string) (float64, bool) {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "agent" && l.GetValue() == agent {
					return m.GetGauge().GetValue(), true
				}
			}
		}
	}
	return 0, false
}

func TestAgentSessionCollector(t *testing.T) {
	store := NewSessionStore()
	a1, a2 := uuid.New(), uuid.New()
	s1 := store.CreateSession(a1, nil, 6, 0)
	s1.IncRunning()
	s1.IncRunning()                          // running=2, capacity=6
	s2 := store.CreateSession(a2, nil, 4, 0) // running=0, capacity=4

	reg := prometheus.NewRegistry()
	reg.MustRegister(NewAgentSessionCollector(store))

	if v, ok := gaugeFor(t, reg, "gocdnext_agent_jobs_running", a1.String()); !ok || v != 2 {
		t.Fatalf("a1 running = %v (ok=%v), want 2", v, ok)
	}
	if v, ok := gaugeFor(t, reg, "gocdnext_agent_capacity", a1.String()); !ok || v != 6 {
		t.Fatalf("a1 capacity = %v (ok=%v), want 6", v, ok)
	}
	if v, ok := gaugeFor(t, reg, "gocdnext_agent_jobs_running", a2.String()); !ok || v != 0 {
		t.Fatalf("a2 running = %v (ok=%v), want 0", v, ok)
	}
	if v, ok := gaugeFor(t, reg, "gocdnext_agent_capacity", a2.String()); !ok || v != 4 {
		t.Fatalf("a2 capacity = %v (ok=%v), want 4", v, ok)
	}

	// A revoked session drops out of the next scrape — no phantom capacity.
	store.Revoke(s2.ID)
	if _, ok := gaugeFor(t, reg, "gocdnext_agent_capacity", a2.String()); ok {
		t.Fatal("revoked session still emits a series")
	}
	if v, ok := gaugeFor(t, reg, "gocdnext_agent_capacity", a1.String()); !ok || v != 6 {
		t.Fatalf("a1 capacity after revoke = %v (ok=%v), want 6", v, ok)
	}
}
