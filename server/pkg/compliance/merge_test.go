package compliance

import (
	"strings"
	"testing"

	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// pipe is a tiny constructor for a domain.Pipeline with stages + named jobs.
func pipe(stages []string, jobs ...domain.Job) domain.Pipeline {
	return domain.Pipeline{Stages: stages, Jobs: jobs}
}

func job(name, stage string) domain.Job { return domain.Job{Name: name, Stage: stage} }

// stageNames / jobNames flatten the result for order-sensitive assertions.
func stageNames(p domain.Pipeline) []string { return p.Stages }
func jobNames(p domain.Pipeline) []string {
	out := make([]string, len(p.Jobs))
	for i, j := range p.Jobs {
		out[i] = j.Name
	}
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestApplyPolicies(t *testing.T) {
	raw := pipe(
		[]string{"build", "test", "deploy"},
		job("compile", "build"), job("unit", "test"), job("ship", "deploy"),
	)

	scanPolicy := Policy{
		Name: "scan", Mode: ModeInject, Priority: 0,
		Pipeline: pipe([]string{"_compliance_scan"}, job("_compliance_scan", "_compliance_scan")),
	}
	gatePolicy := Policy{
		Name: "gate", Mode: ModeInject, Priority: 10,
		PositionBefore: "deploy",
		Pipeline:       pipe([]string{"_compliance_gate"}, job("_compliance_gate", "_compliance_gate")),
	}

	tests := []struct {
		name       string
		raw        domain.Pipeline
		policies   []Policy
		wantStages []string
		wantJobs   []string
	}{
		{
			name:       "no policies returns raw unchanged",
			raw:        raw,
			policies:   nil,
			wantStages: []string{"build", "test", "deploy"},
			wantJobs:   []string{"compile", "unit", "ship"},
		},
		{
			name:       "inject default prepends compliance stage first",
			raw:        raw,
			policies:   []Policy{scanPolicy},
			wantStages: []string{"_compliance_scan", "build", "test", "deploy"},
			wantJobs:   []string{"compile", "unit", "ship", "_compliance_scan"},
		},
		{
			name:       "inject position_before places gate right before deploy",
			raw:        raw,
			policies:   []Policy{gatePolicy},
			wantStages: []string{"build", "test", "_compliance_gate", "deploy"},
			wantJobs:   []string{"compile", "unit", "ship", "_compliance_gate"},
		},
		{
			name: "inject position_after places stage after build",
			raw:  raw,
			policies: []Policy{{
				Name: "p", Mode: ModeInject, PositionAfter: "build",
				Pipeline: pipe([]string{"_compliance_x"}, job("_compliance_x", "_compliance_x")),
			}},
			wantStages: []string{"build", "_compliance_x", "test", "deploy"},
			wantJobs:   []string{"compile", "unit", "ship", "_compliance_x"},
		},
		{
			name: "override replaces repo stages and jobs entirely",
			raw:  raw,
			policies: []Policy{{
				Name: "lockdown", Mode: ModeOverride,
				Pipeline: pipe([]string{"_compliance_only"}, job("_compliance_only", "_compliance_only")),
			}},
			wantStages: []string{"_compliance_only"},
			wantJobs:   []string{"_compliance_only"},
		},
		{
			name:       "multiple policies apply in priority order (scan then gate)",
			raw:        raw,
			policies:   []Policy{gatePolicy, scanPolicy}, // given out of order
			wantStages: []string{"_compliance_scan", "build", "test", "_compliance_gate", "deploy"},
			wantJobs:   []string{"compile", "unit", "ship", "_compliance_scan", "_compliance_gate"},
		},
		{
			name: "stage already present is not duplicated (idempotent)",
			raw:  pipe([]string{"_compliance_scan", "build"}, job("compile", "build")),
			policies: []Policy{{
				Name: "scan", Mode: ModeInject,
				Pipeline: pipe([]string{"_compliance_scan"}, job("_compliance_scan", "_compliance_scan")),
			}},
			wantStages: []string{"_compliance_scan", "build"},
			wantJobs:   []string{"compile", "_compliance_scan"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ApplyPolicies(tt.raw, tt.policies)
			if !eq(stageNames(got), tt.wantStages) {
				t.Errorf("stages = %v, want %v", stageNames(got), tt.wantStages)
			}
			if !eq(jobNames(got), tt.wantJobs) {
				t.Errorf("jobs = %v, want %v", jobNames(got), tt.wantJobs)
			}
			// Identity guarantee: the input raw must not be mutated.
			if len(tt.raw.Stages) > 0 && tt.raw.Stages[0] != raw.Stages[0] && &tt.raw == &raw {
				t.Errorf("input pipeline was mutated")
			}
		})
	}
}

func TestApplyPoliciesStripsLegacyReservedRepoNames(t *testing.T) {
	// A legacy repo pipeline that already carried a reserved-prefixed job
	// (possible via the migration backfill of definition_raw) must NOT be able
	// to shadow a policy job: the repo copy is stripped, the policy's wins.
	raw := domain.Pipeline{
		Stages: []string{"_compliance_scan", "build"},
		Jobs: []domain.Job{
			{Name: "_compliance_scan", Stage: "_compliance_scan", Image: "evil"},
			{Name: "compile", Stage: "build"},
		},
	}
	policy := Policy{
		Name: "scan", Mode: ModeInject,
		Pipeline: domain.Pipeline{
			Stages: []string{"_compliance_scan"},
			Jobs:   []domain.Job{{Name: "_compliance_scan", Stage: "_compliance_scan", Image: "scanner"}},
		},
	}
	got := ApplyPolicies(raw, []Policy{policy})

	// Exactly one _compliance_scan job, and it's the policy's (image=scanner).
	n, img := 0, ""
	for _, j := range got.Jobs {
		if j.Name == "_compliance_scan" {
			n++
			img = j.Image
		}
	}
	if n != 1 || img != "scanner" {
		t.Fatalf("legacy reserved repo job shadowed policy: count=%d image=%q jobs=%v", n, img, jobNames(got))
	}
	// And the reserved stage appears once.
	count := 0
	for _, s := range got.Stages {
		if s == "_compliance_scan" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("reserved stage duplicated: %v", got.Stages)
	}
}

func TestApplyPoliciesDedupesCollidingPolicyJobs(t *testing.T) {
	raw := pipe([]string{"build"}, job("compile", "build"))
	p1 := Policy{Name: "a", Mode: ModeInject, Priority: 0,
		Pipeline: pipe([]string{"_compliance_scan"}, domain.Job{Name: "_compliance_scan", Stage: "_compliance_scan", Image: "first"})}
	p2 := Policy{Name: "b", Mode: ModeInject, Priority: 1,
		Pipeline: pipe([]string{"_compliance_scan"}, domain.Job{Name: "_compliance_scan", Stage: "_compliance_scan", Image: "second"})}
	got := ApplyPolicies(raw, []Policy{p1, p2})
	n := 0
	for _, j := range got.Jobs {
		if j.Name == "_compliance_scan" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("colliding policy jobs not deduped: %v", jobNames(got))
	}
}

func TestApplyPoliciesDoesNotMutateInput(t *testing.T) {
	raw := pipe([]string{"build"}, job("compile", "build"))
	_ = ApplyPolicies(raw, []Policy{{
		Name: "scan", Mode: ModeInject,
		Pipeline: pipe([]string{"_compliance_scan"}, job("_compliance_scan", "_compliance_scan")),
	}})
	if len(raw.Stages) != 1 || len(raw.Jobs) != 1 {
		t.Fatalf("input mutated: stages=%v jobs=%d", raw.Stages, len(raw.Jobs))
	}
}

func TestRejectReservedNames(t *testing.T) {
	if err := RejectReservedNames(pipe([]string{"build"}, job("compile", "build"))); err != nil {
		t.Errorf("clean repo pipeline rejected: %v", err)
	}
	if err := RejectReservedNames(pipe([]string{"_compliance_x"})); err == nil {
		t.Error("repo stage using reserved prefix should be rejected")
	}
	if err := RejectReservedNames(pipe([]string{"build"}, job("_compliance_evil", "build"))); err == nil {
		t.Error("repo job using reserved prefix should be rejected")
	}
}

func TestCompilePolicy(t *testing.T) {
	good := `
stages: [_compliance_scan]
jobs:
  _compliance_scan:
    stage: _compliance_scan
    image: scanner:latest
    script: ["scan ."]
`
	p, err := CompilePolicy(good)
	if err != nil {
		t.Fatalf("valid policy failed to compile: %v", err)
	}
	if !eq(p.Stages, []string{"_compliance_scan"}) {
		t.Errorf("stages = %v", p.Stages)
	}
	if len(p.Jobs) != 1 || p.Jobs[0].Name != "_compliance_scan" {
		t.Errorf("jobs = %v", jobNames(p))
	}

	// A stage without the reserved prefix must be rejected.
	badStage := `
stages: [scan]
jobs:
  _compliance_scan:
    stage: scan
    image: x
    script: ["s"]
`
	if _, err := CompilePolicy(badStage); err == nil {
		t.Error("policy stage without reserved prefix should be rejected")
	} else if !strings.Contains(err.Error(), ReservedPrefix) {
		t.Errorf("error should mention the reserved prefix, got: %v", err)
	}

	// A job without the reserved prefix must be rejected.
	badJob := `
stages: [_compliance_scan]
jobs:
  scan:
    stage: _compliance_scan
    image: x
    script: ["s"]
`
	if _, err := CompilePolicy(badJob); err == nil {
		t.Error("policy job without reserved prefix should be rejected")
	}

	// Invalid YAML / schema must error, not panic.
	if _, err := CompilePolicy("this: : not yaml"); err == nil {
		t.Error("invalid YAML should error")
	}
}
