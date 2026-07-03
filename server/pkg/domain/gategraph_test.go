package domain

import (
	"reflect"
	"testing"
)

func gate(name, stage string, needs ...string) Job {
	return Job{Name: name, Stage: stage, Needs: needs, Approval: &ApprovalSpec{}}
}
func deploy(name, stage, env string, needs ...string) Job {
	return Job{Name: name, Stage: stage, Needs: needs, Deploy: &DeploySpec{Environment: env}}
}

func TestReadyGateEnvsAtStart(t *testing.T) {
	tests := []struct {
		name      string
		p         *Pipeline
		wantEnvs  []string
		wantReady bool
	}{
		{
			name: "gate-first pipeline: stage-0 staging gate is ready",
			p: &Pipeline{
				Stages: []string{"approve-staging", "deploy-staging", "approve-prod", "deploy-prod"},
				Jobs: []Job{
					gate("approve-staging", "approve-staging"),
					deploy("deploy-staging", "deploy-staging", "staging"),
					gate("approve-prod", "approve-prod"),
					deploy("deploy-prod", "deploy-prod", "prod"),
				},
			},
			wantEnvs: []string{"staging"}, wantReady: true, // prod gate is stage 2, NOT ready at start
		},
		{
			name: "build-first pipeline: stage-0 has no gate → not ready",
			p: &Pipeline{
				Stages: []string{"build", "approve-staging", "deploy-staging"},
				Jobs: []Job{
					{Name: "compile", Stage: "build"},
					gate("approve-staging", "approve-staging"),
					deploy("deploy-staging", "deploy-staging", "staging"),
				},
			},
			wantEnvs: nil, wantReady: false,
		},
		{
			name: "stage-0 gate with needs → not ready at start",
			p: &Pipeline{
				Stages: []string{"first", "deploy-prod"},
				Jobs: []Job{
					{Name: "seed", Stage: "first"},
					gate("approve", "first", "seed"), // needs seed → not reachable at creation
					deploy("deploy-prod", "deploy-prod", "prod"),
				},
			},
			wantEnvs: nil, wantReady: false,
		},
		{
			name: "pure-approval stage-0 gate: ready, governs no deploy (whole-run scope)",
			p: &Pipeline{
				Stages: []string{"approve"},
				Jobs:   []Job{gate("approve", "approve")},
			},
			wantEnvs: nil, wantReady: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			envs, ready := tt.p.ReadyGateEnvsAtStart()
			if ready != tt.wantReady || !reflect.DeepEqual(envs, tt.wantEnvs) {
				t.Fatalf("ReadyGateEnvsAtStart() = (%v, %v), want (%v, %v)", envs, ready, tt.wantEnvs, tt.wantReady)
			}
		})
	}
}

func TestGateGraph_LinearStagingProd(t *testing.T) {
	p := &Pipeline{
		Stages: []string{"build", "approve-staging", "deploy-staging", "approve-prod", "deploy-prod"},
		Jobs: []Job{
			{Name: "compile", Stage: "build"},
			gate("approve-staging", "approve-staging"),
			deploy("deploy-staging", "deploy-staging", "staging"),
			gate("approve-prod", "approve-prod"),
			deploy("deploy-prod", "deploy-prod", "prod"),
		},
	}
	if got := p.GovernedEnvs("approve-staging"); !reflect.DeepEqual(got, []string{"staging"}) {
		t.Fatalf("GovernedEnvs(approve-staging) = %v, want [staging]", got)
	}
	if got := p.GovernedEnvs("approve-prod"); !reflect.DeepEqual(got, []string{"prod"}) {
		t.Fatalf("GovernedEnvs(approve-prod) = %v, want [prod] (staging gate must NOT govern prod)", got)
	}
	if got := p.GoverningGates("prod"); !reflect.DeepEqual(got, []string{"approve-prod"}) {
		t.Fatalf("GoverningGates(prod) = %v, want [approve-prod]", got)
	}
	if got := p.GoverningGates("staging"); !reflect.DeepEqual(got, []string{"approve-staging"}) {
		t.Fatalf("GoverningGates(staging) = %v, want [approve-staging]", got)
	}
}

func TestGateGraph_SameStageNoNeeds_NotGoverned(t *testing.T) {
	p := &Pipeline{
		Stages: []string{"release"},
		Jobs: []Job{
			gate("approve", "release"),
			deploy("deploy-prod", "release", "prod"), // same stage, no needs → parallel, not governed
		},
	}
	if got := p.GovernedEnvs("approve"); len(got) != 0 {
		t.Fatalf("same-stage deploy without needs must NOT be governed, got %v", got)
	}
}

func TestGateGraph_SameStageWithNeeds_Governed(t *testing.T) {
	p := &Pipeline{
		Stages: []string{"release"},
		Jobs: []Job{
			gate("approve", "release"),
			deploy("deploy-prod", "release", "prod", "approve"), // needs the gate → governed
		},
	}
	if got := p.GovernedEnvs("approve"); !reflect.DeepEqual(got, []string{"prod"}) {
		t.Fatalf("same-stage deploy WITH needs must be governed, got %v", got)
	}
}

func TestGateGraph_LaterStageNoNeeds_GovernedViaSequence(t *testing.T) {
	p := &Pipeline{
		Stages: []string{"approve", "deploy"},
		Jobs: []Job{
			gate("approve", "approve"),
			deploy("deploy-prod", "deploy", "prod"), // later stage, no needs → governed by sequence
		},
	}
	if got := p.GovernedEnvs("approve"); !reflect.DeepEqual(got, []string{"prod"}) {
		t.Fatalf("later-stage deploy must be governed via stage sequence, got %v", got)
	}
}

func TestGateGraph_FanOut(t *testing.T) {
	p := &Pipeline{
		Stages: []string{"approve", "deploy"},
		Jobs: []Job{
			gate("approve", "approve"),
			deploy("deploy-staging", "deploy", "staging"),
			deploy("deploy-prod", "deploy", "prod"),
		},
	}
	if got := p.GovernedEnvs("approve"); !reflect.DeepEqual(got, []string{"prod", "staging"}) {
		t.Fatalf("fan-out gate must govern both envs, got %v", got)
	}
}

func TestGateGraph_MultiGateForOneEnv(t *testing.T) {
	p := &Pipeline{
		Stages: []string{"approve", "deploy"},
		Jobs: []Job{
			gate("approve-security", "approve"),
			gate("approve-ops", "approve"),
			deploy("deploy-prod", "deploy", "prod", "approve-security", "approve-ops"),
		},
	}
	if got := p.GoverningGates("prod"); !reflect.DeepEqual(got, []string{"approve-ops", "approve-security"}) {
		t.Fatalf("multi-gate env must list both governing gates, got %v", got)
	}
}

func TestGateGraph_GateChainPerPath(t *testing.T) {
	// approve-security → approve-prod → deploy-prod (all wired by needs, gates in
	// the SAME stage). The next gate on the path cuts propagation, so ONLY
	// approve-prod governs prod — approve-security is shadowed via the needs chain,
	// which a stage-only shadow rule would have missed.
	p := &Pipeline{
		Stages: []string{"approve", "deploy"},
		Jobs: []Job{
			gate("approve-security", "approve"),
			gate("approve-prod", "approve", "approve-security"),
			deploy("deploy-prod", "deploy", "prod", "approve-prod"),
		},
	}
	if got := p.GoverningGates("prod"); !reflect.DeepEqual(got, []string{"approve-prod"}) {
		t.Fatalf("GoverningGates(prod) = %v, want [approve-prod] (security shadowed via needs chain)", got)
	}
	if got := p.GovernedEnvs("approve-security"); len(got) != 0 {
		t.Fatalf("approve-security governs nothing (shadowed), got %v", got)
	}
	if got := p.GovernedEnvs("approve-prod"); !reflect.DeepEqual(got, []string{"prod"}) {
		t.Fatalf("GovernedEnvs(approve-prod) = %v, want [prod]", got)
	}
}

func TestGateGraph_GateGovernsNoDeploy(t *testing.T) {
	p := &Pipeline{
		Stages: []string{"approve"},
		Jobs:   []Job{gate("approve", "approve")}, // pure-approval pipeline, no deploy
	}
	if got := p.GovernedEnvs("approve"); len(got) != 0 {
		t.Fatalf("gate governing no deploy must have empty env set, got %v", got)
	}
}
