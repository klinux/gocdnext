package store_test

import (
	"testing"

	"github.com/google/uuid"
)

// TestListArmedRolloutGatesForCluster exercises the dashboard correlation query end to
// end against a real Postgres (testcontainers via seedGatedRollout): the armed/undecided
// filter, the cluster + project scoping, the approvals_now count, and the exclusion of a
// gate once it is decided. seedGatedRollout arms the gate with the pinned identity
// cluster="dest", namespace="ns", name="ro" and quorum from `required`.
func TestListArmedRolloutGatesForCluster(t *testing.T) {
	gr := seedGatedRollout(t, "rg-correlate", 2, []string{"alice@corp.com", "bob@corp.com"})

	// Freshly armed, no votes yet: exactly one gate on the pinned cluster, quorum 0/2.
	got, err := gr.s.ListArmedRolloutGatesForCluster(gr.ctx, gr.projectID, "dest")
	if err != nil {
		t.Fatalf("list armed gates: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1 armed gate; got %+v", len(got), got)
	}
	g := got[0]
	if g.GateID != gr.gateID || g.RevisionID != gr.revID {
		t.Errorf("identity = gate %s rev %s, want gate %s rev %s", g.GateID, g.RevisionID, gr.gateID, gr.revID)
	}
	if g.Namespace != "ns" || g.Name != "ro" {
		t.Errorf("pinned identity = %s/%s, want ns/ro", g.Namespace, g.Name)
	}
	if g.Required != 2 || g.ApprovalsNow != 0 {
		t.Errorf("quorum = %d/%d, want 0/2", g.ApprovalsNow, g.Required)
	}

	// Scoping: a different cluster or a different project sees nothing (no cross-project
	// or cross-cluster leak of an armed gate).
	scopes := []struct {
		name    string
		project uuid.UUID
		cluster string
	}{
		{"wrong cluster", gr.projectID, "other"},
		{"empty cluster (fail-safe, never matches a NULL/blank pin)", gr.projectID, ""},
		{"wrong project", uuid.New(), "dest"},
	}
	for _, sc := range scopes {
		t.Run(sc.name, func(t *testing.T) {
			rows, err := gr.s.ListArmedRolloutGatesForCluster(gr.ctx, sc.project, sc.cluster)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if len(rows) != 0 {
				t.Fatalf("len = %d, want 0; got %+v", len(rows), rows)
			}
		})
	}

	// One approve (quorum 2 not met): the gate stays armed + undecided, approvals_now=1.
	alice := gr.user(t, "alice@corp.com", "Alice")
	if res, err := gr.decide(alice, "alice@corp.com", "Alice", "approved"); err != nil || !res.PendingQuorum {
		t.Fatalf("first approve = %+v err=%v, want PendingQuorum", res, err)
	}
	got, err = gr.s.ListArmedRolloutGatesForCluster(gr.ctx, gr.projectID, "dest")
	if err != nil {
		t.Fatalf("list after partial approve: %v", err)
	}
	if len(got) != 1 || got[0].ApprovalsNow != 1 {
		t.Fatalf("after partial approve = %+v, want 1 gate approvals_now=1", got)
	}

	// Quorum met (second distinct approver) → decided. A decided gate's Approve/Reject
	// window is closed, so it drops out of the correlation.
	bob := gr.user(t, "bob@corp.com", "Bob")
	if res, err := gr.decide(bob, "bob@corp.com", "Bob", "approved"); err != nil || !res.Decided {
		t.Fatalf("quorum approve = %+v err=%v, want Decided", res, err)
	}
	got, err = gr.s.ListArmedRolloutGatesForCluster(gr.ctx, gr.projectID, "dest")
	if err != nil {
		t.Fatalf("list after decision: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("decided gate still listed: %+v", got)
	}
}
