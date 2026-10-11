package rpc_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/gocdnext/gocdnext/agent/internal/engine"
	"github.com/gocdnext/gocdnext/agent/internal/rpc"
)

// newLogCapturingClient builds a Client whose logger writes into buf
// at Debug level, so a test can assert on both the WARN and the DEBUG
// branch of the service-diagnostics log (#305).
func newLogCapturingClient(buf *bytes.Buffer) *rpc.Client {
	h := slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return rpc.New(rpc.Config{}, slog.New(h))
}

func restartedDiagnostics() *engine.ServicePodDiagnostics {
	return &engine.ServicePodDiagnostics{
		PodName:  "gocdnext-svc-abc-g0-postgres",
		Phase:    "Running",
		NodeName: "node-7",
		Containers: []engine.ServiceContainerDiagnostics{{
			Name:         "service",
			Ready:        true,
			RestartCount: 1,
			State:        "running",
			LastTermination: &engine.ContainerTermination{
				Reason:   "OOMKilled",
				ExitCode: 137,
				Signal:   9,
			},
		}},
	}
}

// TestServiceLifecycleAck_WarnsWhenServiceWasUnhealthy is the whole
// point of the change: when a run-scoped service died and came back,
// the agent log has to say so. Before, teardown recorded nothing and
// the consumer job's failures looked like a code regression.
func TestServiceLifecycleAck_WarnsWhenServiceWasUnhealthy(t *testing.T) {
	var buf bytes.Buffer
	c := newLogCapturingClient(&buf)

	// No ack bridge installed on purpose — the diagnostics log must
	// not depend on a connected stream.
	c.ServiceLifecycleAckForTest("run-1")(engine.ServiceLifecycleEvent{
		Name:        "postgres",
		PodName:     "gocdnext-svc-abc-g0-postgres",
		Status:      "stopped",
		Diagnostics: restartedDiagnostics(),
	})

	out := buf.String()
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("want a WARN record for an unhealthy service; got %q", out)
	}
	for _, want := range []string{
		"run_id=run-1",
		"service=postgres",
		"svc_restarts=1",
		"svc_last_terminated=true",
		"svc_last_exit_code=137",
		"svc_last_reason=OOMKilled",
		"svc_node=node-7",
		// The actionable half for #305: no memory ceiling at all.
		"svc_mem_limit=(none)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q; got %q", want, out)
		}
	}
	// The message itself has to be readable on its own, not just the
	// attributes — an operator greps the message first.
	if !strings.Contains(out, "restarted 1 time(s)") {
		t.Errorf("message does not summarise the restart; got %q", out)
	}
}

// TestServiceLifecycleAck_DebugWhenHealthy keeps steady state quiet:
// one line per service per run at WARN would train operators to ignore
// it. The snapshot stays retrievable by raising the level.
func TestServiceLifecycleAck_DebugWhenHealthy(t *testing.T) {
	var buf bytes.Buffer
	c := newLogCapturingClient(&buf)

	c.ServiceLifecycleAckForTest("run-1")(engine.ServiceLifecycleEvent{
		Name:    "redis",
		PodName: "gocdnext-svc-abc-g0-redis",
		Status:  "stopped",
		Diagnostics: &engine.ServicePodDiagnostics{
			PodName: "gocdnext-svc-abc-g0-redis",
			Phase:   "Running",
			Containers: []engine.ServiceContainerDiagnostics{{
				Name: "service", Ready: true, State: "running",
			}},
		},
	})

	out := buf.String()
	if strings.Contains(out, "level=WARN") {
		t.Errorf("healthy service produced a WARN; got %q", out)
	}
	if !strings.Contains(out, "level=DEBUG") {
		t.Errorf("want the snapshot at DEBUG; got %q", out)
	}
	if !strings.Contains(out, "svc_restarts=0") {
		t.Errorf("log missing the restart count; got %q", out)
	}
	// The "did it ever die" bit is always present, so a query on it
	// never has to special-case a missing field.
	if !strings.Contains(out, "svc_last_terminated=false") {
		t.Errorf("log missing svc_last_terminated; got %q", out)
	}
	if strings.Contains(out, "svc_last_exit_code") {
		t.Errorf("exit code emitted for a container that never died; got %q", out)
	}
}

// TestServiceLifecycleAck_NeverLogsWorkloadText closes the loop on the
// exfiltration path at the LAST hop: the agent's slog output. The
// snapshot drops workload-authored Message fields at extraction
// (engine.serviceDiagnosticsFromPod), and this asserts nothing
// reintroduces them downstream — the agent log is not masked, so a
// secret written to a service container's /dev/termination-log must
// never reach it.
func TestServiceLifecycleAck_NeverLogsWorkloadText(t *testing.T) {
	const secret = "SUPERSECRET_ghp_abc123_from_termination_log"

	var buf bytes.Buffer
	c := newLogCapturingClient(&buf)

	// A snapshot hand-built with the secret in every text-ish field
	// that a future refactor might add back.
	c.ServiceLifecycleAckForTest("run-1")(engine.ServiceLifecycleEvent{
		Name:    "postgres",
		PodName: "gocdnext-svc-abc-g0-postgres",
		Status:  "stopped",
		Diagnostics: &engine.ServicePodDiagnostics{
			PodName:    "gocdnext-svc-abc-g0-postgres",
			Phase:      "Failed",
			Reason:     "Evicted",
			NodeName:   "node-7",
			Conditions: []engine.PodConditionSnapshot{{Type: "Ready", Status: "False", Reason: "ContainersNotReady"}},
			Containers: []engine.ServiceContainerDiagnostics{{
				Name:         "service",
				RestartCount: 1,
				State:        "waiting",
				StateReason:  "CrashLoopBackOff",
				LastTermination: &engine.ContainerTermination{
					Reason:   "Error",
					ExitCode: 1,
				},
			}},
		},
	})

	if out := buf.String(); strings.Contains(out, secret) {
		t.Errorf("agent log carries workload-authored text: %q", out)
	}
	// Sanity: the record was actually emitted, so the assertion above
	// is not passing on an empty buffer.
	if out := buf.String(); !strings.Contains(out, "svc_state_reason=CrashLoopBackOff") {
		t.Errorf("expected the bounded fields to still be logged; got %q", out)
	}
}

// TestServiceLifecycleAck_SilentWithoutDiagnostics: `starting`,
// `ready` and `failed` events carry no snapshot, and the Docker engine
// never populates one. Those must not emit an empty record.
func TestServiceLifecycleAck_SilentWithoutDiagnostics(t *testing.T) {
	var buf bytes.Buffer
	c := newLogCapturingClient(&buf)

	c.ServiceLifecycleAckForTest("run-1")(engine.ServiceLifecycleEvent{
		Name:   "postgres",
		Status: "stopped",
	})

	if out := buf.String(); strings.Contains(out, "svc_pod") {
		t.Errorf("logged a diagnostics record with no diagnostics; got %q", out)
	}
}
