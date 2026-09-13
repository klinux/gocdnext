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

// TestBuildAssignment_CarriesServiceGeneration proves the run's service_generation
// reaches the agent on the JobAssignment (#97) — the k8s engine needs it to name+label
// service pods per generation, so a revived run's pods survive a stale cleanup.
func TestBuildAssignment_CarriesServiceGeneration(t *testing.T) {
	def := domain.Pipeline{
		Stages: []string{"ci"},
		Jobs:   []domain.Job{{Name: "build", Stage: "ci", Image: "alpine:3.19"}},
	}
	defJSON, err := json.Marshal(def)
	if err != nil {
		t.Fatalf("marshal def: %v", err)
	}
	asg, _, err := scheduler.BuildAssignment(
		store.RunForDispatch{Definition: defJSON, ServiceGeneration: 5},
		store.DispatchableJob{Name: "build"},
		nil, nil, nil, store.ResolvedProfile{}, nil, nil, nil, nil, "", nil,
	)
	if err != nil {
		t.Fatalf("BuildAssignment: %v", err)
	}
	if asg.GetServiceGeneration() != 5 {
		t.Errorf("JobAssignment.service_generation = %d, want 5 (threaded from the run)", asg.GetServiceGeneration())
	}
}

// TestBuildAssignment_ServiceSchedulingOverride proves a service's per-service
// node_selector + tolerations survive the definition → JobAssignment proto hop
// (including toleration_seconds via the optional field).
func TestBuildAssignment_ServiceSchedulingOverride(t *testing.T) {
	secs := int64(30)
	def := domain.Pipeline{
		Stages: []string{"ci"},
		Jobs:   []domain.Job{{Name: "build", Stage: "ci", Image: "alpine:3.19"}},
		Services: []domain.Service{{
			Name:         "postgres",
			Image:        "postgres:16",
			NodeSelector: map[string]string{"cloud.google.com/gke-nodepool": "ondemand"},
			Tolerations: []domain.Toleration{
				{Key: "dedicated", Operator: "Equal", Value: "db", Effect: "NoSchedule"},
				{Key: "spot", Operator: "Exists", Effect: "NoExecute", TolerationSeconds: &secs},
			},
		}},
	}
	defJSON, err := json.Marshal(def)
	if err != nil {
		t.Fatalf("marshal def: %v", err)
	}
	asg, _, err := scheduler.BuildAssignment(
		store.RunForDispatch{Definition: defJSON},
		store.DispatchableJob{Name: "build"},
		nil, nil, nil, store.ResolvedProfile{}, nil, nil, nil, nil, "", nil,
	)
	if err != nil {
		t.Fatalf("BuildAssignment: %v", err)
	}
	svcs := asg.GetServices()
	if len(svcs) != 1 {
		t.Fatalf("services = %d, want 1", len(svcs))
	}
	svc := svcs[0]
	if svc.GetNodeSelector()["cloud.google.com/gke-nodepool"] != "ondemand" {
		t.Errorf("ServiceSpec.node_selector = %v", svc.GetNodeSelector())
	}
	tols := svc.GetTolerations()
	if len(tols) != 2 {
		t.Fatalf("ServiceSpec.tolerations = %d, want 2", len(tols))
	}
	var spotFound bool
	for _, tol := range tols {
		if tol.GetKey() == "spot" {
			spotFound = true
			if tol.TolerationSeconds == nil || *tol.TolerationSeconds != 30 {
				t.Errorf("spot toleration_seconds = %v, want 30", tol.TolerationSeconds)
			}
		}
	}
	if !spotFound {
		t.Error("spot toleration missing on the wire")
	}
}

// TestBuildAssignment_ProfileStorageThreaded proves the per-profile workspace
// + DinD storage sizing survives the ResolvedProfile → JobAssignment proto hop.
// The agent reads these to size the isolated-mode pod's PVCs
// (feat/runner-profile-dind-storage).
func TestBuildAssignment_ProfileStorageThreaded(t *testing.T) {
	def := domain.Pipeline{
		Stages: []string{"ci"},
		Jobs:   []domain.Job{{Name: "build", Stage: "ci", Image: "docker:24", Docker: true}},
	}
	defJSON, err := json.Marshal(def)
	if err != nil {
		t.Fatalf("marshal def: %v", err)
	}
	profile := store.ResolvedProfile{
		WorkspaceSize:         "200Gi",
		WorkspaceStorageClass: "local-ssd",
		DinDStorageSize:       "300Gi",
		DinDStorageClass:      "premium-rwo",
	}
	asg, _, err := scheduler.BuildAssignment(
		store.RunForDispatch{Definition: defJSON},
		store.DispatchableJob{Name: "build"},
		nil, nil, nil, profile, nil, nil, nil, nil, "", nil,
	)
	if err != nil {
		t.Fatalf("BuildAssignment: %v", err)
	}
	if got := asg.GetWorkspaceSize(); got != "200Gi" {
		t.Errorf("workspace_size = %q, want 200Gi", got)
	}
	if got := asg.GetWorkspaceStorageClass(); got != "local-ssd" {
		t.Errorf("workspace_storage_class = %q, want local-ssd", got)
	}
	if got := asg.GetDindStorageSize(); got != "300Gi" {
		t.Errorf("dind_storage_size = %q, want 300Gi", got)
	}
	if got := asg.GetDindStorageClass(); got != "premium-rwo" {
		t.Errorf("dind_storage_class = %q, want premium-rwo", got)
	}
}

// TestBuildAssignment_NoProfileStorageIsEmpty: a profile without storage
// sizing leaves the assignment fields empty (agent falls back to its own
// defaults / node-disk).
func TestBuildAssignment_NoProfileStorageIsEmpty(t *testing.T) {
	def := domain.Pipeline{
		Stages: []string{"ci"},
		Jobs:   []domain.Job{{Name: "build", Stage: "ci", Image: "alpine:3.19"}},
	}
	defJSON, _ := json.Marshal(def)
	asg, _, err := scheduler.BuildAssignment(
		store.RunForDispatch{Definition: defJSON},
		store.DispatchableJob{Name: "build"},
		nil, nil, nil, store.ResolvedProfile{}, nil, nil, nil, nil, "", nil,
	)
	if err != nil {
		t.Fatalf("BuildAssignment: %v", err)
	}
	if asg.GetWorkspaceSize() != "" || asg.GetDindStorageSize() != "" ||
		asg.GetWorkspaceStorageClass() != "" || asg.GetDindStorageClass() != "" {
		t.Errorf("expected empty storage fields; got ws=%q/%q dind=%q/%q",
			asg.GetWorkspaceSize(), asg.GetWorkspaceStorageClass(),
			asg.GetDindStorageSize(), asg.GetDindStorageClass())
	}
}
