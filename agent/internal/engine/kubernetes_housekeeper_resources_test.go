package engine

import (
	"testing"

	"k8s.io/client-go/kubernetes/fake"
)

// TestHousekeeperLimits covers the configurable housekeeper CPU/memory limits
// (#274): the cache tar+compress runs in this sidecar, so zstd -T0 needs the
// CPU limit raised. Default stays the historical 1/512Mi; a valid override
// flows through; a garbage value fails safe to the default instead of
// panicking MustParse during pod construction.
func TestHousekeeperLimits(t *testing.T) {
	build := func(cpu, mem string) (cpuLimit, memLimit string) {
		k := NewKubernetesWithClient(fake.NewSimpleClientset(), KubernetesConfig{
			Namespace:           "ci",
			WorkspaceMode:       WorkspaceModeIsolated,
			WorkspaceMountPath:  "/workspace",
			AgentImage:          "ghcr.io/klinux/gocdnext-agent:test",
			HousekeeperImage:    "gocdnext/housekeeper:test",
			DefaultImage:        "alpine:3.19",
			HousekeeperCPULimit: cpu,
			HousekeeperMemLimit: mem,
		})
		k.nowName = func() string { return "gocdnext-job-test01" }
		pod, err := k.BuildIsolatedJobPodSpec(IsolatedJobSpec{
			RunID: "r", JobID: "j", Image: "node:20", Script: "true",
			WorkDir: "/workspace", AssignmentSecretName: "s",
		})
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		hk := findContainer(pod.Spec.Containers, "housekeeper")
		if hk == nil {
			t.Fatal("no housekeeper container")
		}
		return hk.Resources.Limits.Cpu().String(), hk.Resources.Limits.Memory().String()
	}

	tests := []struct {
		name, cpu, mem, wantCPU, wantMem string
	}{
		{"defaults", "", "", "1", "512Mi"},
		{"raised for zstd", "4", "1Gi", "4", "1Gi"},
		{"garbage cpu falls back", "not-a-quantity", "", "1", "512Mi"},
		{"garbage mem falls back", "2", "banana", "2", "512Mi"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotCPU, gotMem := build(tt.cpu, tt.mem)
			if gotCPU != tt.wantCPU {
				t.Errorf("cpu limit = %q, want %q", gotCPU, tt.wantCPU)
			}
			if gotMem != tt.wantMem {
				t.Errorf("mem limit = %q, want %q", gotMem, tt.wantMem)
			}
		})
	}

	// The idle request must stay tiny regardless of the limit, so raising
	// the limit for zstd doesn't change how the pod is scheduled.
	k := NewKubernetesWithClient(fake.NewSimpleClientset(), KubernetesConfig{
		Namespace: "ci", WorkspaceMode: WorkspaceModeIsolated, WorkspaceMountPath: "/workspace",
		AgentImage: "a", HousekeeperImage: "h", DefaultImage: "d", HousekeeperCPULimit: "4",
	})
	k.nowName = func() string { return "gocdnext-job-test01" }
	pod, err := k.BuildIsolatedJobPodSpec(IsolatedJobSpec{
		RunID: "r", JobID: "j", Image: "node:20", Script: "true",
		WorkDir: "/workspace", AssignmentSecretName: "s",
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	hk := findContainer(pod.Spec.Containers, "housekeeper")
	if got := hk.Resources.Requests.Cpu().String(); got != "10m" {
		t.Errorf("cpu request should stay 10m regardless of limit, got %q", got)
	}
}
