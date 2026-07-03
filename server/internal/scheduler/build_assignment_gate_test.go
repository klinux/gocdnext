package scheduler_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gocdnext/gocdnext/server/internal/scheduler"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// HIGH #1 layer 3 (last line): BuildAssignment refuses an approval gate, so even if
// RerunJob's guard and the dispatch query's filter were both bypassed, a gate can
// never be turned into a JobAssignment and "pass" as a task-less job.
func TestBuildAssignment_RefusesApprovalGate(t *testing.T) {
	def := domain.Pipeline{
		Stages: []string{"approve"},
		Jobs:   []domain.Job{{Name: "gate", Stage: "approve", Approval: &domain.ApprovalSpec{}}},
	}
	defJSON, err := json.Marshal(def)
	if err != nil {
		t.Fatalf("marshal def: %v", err)
	}
	_, _, err = scheduler.BuildAssignment(
		store.RunForDispatch{Definition: defJSON},
		store.DispatchableJob{Name: "gate"},
		nil, nil, nil, store.ResolvedProfile{}, nil, nil, nil, nil, "", nil,
	)
	if err == nil || !strings.Contains(err.Error(), "approval gate") {
		t.Fatalf("BuildAssignment for an approval gate = %v, want a refusal", err)
	}
}
