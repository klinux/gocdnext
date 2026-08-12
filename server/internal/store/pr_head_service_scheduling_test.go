package store

import (
	"testing"

	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// The PR-head is contributor-controlled and bypasses ApplyProject, so a service
// it declares must NOT carry a scheduling override — node placement is
// admin-managed. stripHeadServiceScheduling drops node_selector + tolerations
// (keeping everything else) and must not mutate the caller's slice.
func TestStripHeadServiceScheduling(t *testing.T) {
	secs := int64(30)
	in := []domain.Service{{
		Name:         "postgres",
		Image:        "postgres:16",
		Env:          map[string]string{"POSTGRES_PASSWORD": "x"},
		Command:      []string{"-c", "fsync=off"},
		NodeSelector: map[string]string{"cloud.google.com/gke-nodepool": "prod"},
		Tolerations: []domain.Toleration{
			// the nastiest case: Exists with no key tolerates ALL taints.
			{Operator: "Exists", Effect: "NoExecute", TolerationSeconds: &secs},
		},
	}}

	out := stripHeadServiceScheduling(in)
	if len(out) != 1 {
		t.Fatalf("services = %d, want 1", len(out))
	}
	s := out[0]
	if s.NodeSelector != nil {
		t.Errorf("NodeSelector not stripped: %v", s.NodeSelector)
	}
	if s.Tolerations != nil {
		t.Errorf("Tolerations not stripped: %v", s.Tolerations)
	}
	// Non-scheduling fields survive — the service still runs.
	if s.Name != "postgres" || s.Image != "postgres:16" ||
		s.Env["POSTGRES_PASSWORD"] != "x" || len(s.Command) != 2 {
		t.Errorf("non-scheduling fields mangled: %+v", s)
	}
	// The caller's slice must be untouched (helper returns a copy).
	if in[0].NodeSelector == nil || in[0].Tolerations == nil {
		t.Error("input slice was mutated; helper must return a copy")
	}
}

func TestStripHeadServiceScheduling_Empty(t *testing.T) {
	if got := stripHeadServiceScheduling(nil); got != nil {
		t.Errorf("nil in → %v, want nil", got)
	}
}
