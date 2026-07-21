package deploy

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func ptr(i int) *int { return &i }

func TestRolloutView_Parse(t *testing.T) {
	// (a) A canary mid-rollout: setWeight 25 / pause{} / setWeight 50 /
	// pause{duration:10} / setWeight 100, a reported canary weight, a KNOWN current
	// step, and a step AnalysisRun.
	const canaryMid = `{
      "metadata":{"namespace":"prod","name":"checkout"},
      "spec":{
        "strategy":{"canary":{"steps":[
          {"setWeight":25},{"pause":{}},{"setWeight":50},{"pause":{"duration":10}},{"setWeight":100}
        ]}},
        "template":{"spec":{"containers":[{"image":"registry.example.com/checkout:v2"}]}}
      },
      "status":{
        "phase":"Paused","message":"CanaryPauseStep","currentStepIndex":3,
        "stableRS":"abc123","currentPodHash":"def456",
        "canary":{
          "weights":{"canary":{"weight":50}},
          "currentStepAnalysisRunStatus":{"name":"checkout-abc-3","status":"Running","message":"in progress"}
        }
      }
    }`

	// (b) A canary with an ABSENT currentStepIndex — must NOT be trusted as step 0.
	const canaryPending = `{
      "metadata":{"namespace":"prod","name":"pending-ro"},
      "spec":{
        "strategy":{"canary":{"steps":[{"setWeight":10},{"pause":{}}]}},
        "template":{"spec":{"containers":[{"image":"img:1"}]}}
      },
      "status":{"phase":"Progressing"}
    }`

	// (c) A blue-green rollout — Strategy blueGreen, no canary steps.
	const blueGreen = `{
      "metadata":{"namespace":"prod","name":"bg-ro"},
      "spec":{
        "strategy":{"blueGreen":{"activeService":"active","previewService":"preview"}},
        "template":{"spec":{"containers":[{"image":"img:bg"}]}}
      },
      "status":{"phase":"Healthy","stableRS":"s1","currentPodHash":"s1"}
    }`

	tests := []struct {
		name string
		raw  string
		want RolloutView
	}{
		{
			name: "canary mid-rollout, every field",
			raw:  canaryMid,
			want: RolloutView{
				Namespace: "prod", Name: "checkout", Strategy: "canary",
				Phase: RolloutPaused, Message: "CanaryPauseStep", Aborted: false,
				CurrentStepIndex: 3, CurrentStepKnown: true,
				Steps: []RolloutViewStep{
					{Kind: "setWeight", Weight: ptr(25)},
					{Kind: "pause", PauseDuration: ""},
					{Kind: "setWeight", Weight: ptr(50)},
					{Kind: "pause", PauseDuration: "10"},
					{Kind: "setWeight", Weight: ptr(100)},
				},
				CanaryWeight: 50, StableHash: "abc123", PodHash: "def456",
				Image:    "registry.example.com/checkout:v2",
				Analysis: &RolloutAnalysis{Name: "checkout-abc-3", Phase: AnalysisRunning, Message: "in progress"},
			},
		},
		{
			name: "canary with absent currentStepIndex -> unknown, not step 0",
			raw:  canaryPending,
			want: RolloutView{
				Namespace: "prod", Name: "pending-ro", Strategy: "canary",
				Phase:            RolloutProgressing,
				CurrentStepIndex: 0, CurrentStepKnown: false,
				Steps: []RolloutViewStep{
					{Kind: "setWeight", Weight: ptr(10)},
					{Kind: "pause", PauseDuration: ""},
				},
				Image: "img:1",
			},
		},
		{
			name: "blue-green rollout has blueGreen strategy and no steps",
			raw:  blueGreen,
			want: RolloutView{
				Namespace: "prod", Name: "bg-ro", Strategy: "blueGreen",
				Phase:      RolloutHealthy,
				StableHash: "s1", PodHash: "s1", Image: "img:bg",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseRolloutView([]byte(tt.raw))
			if err != nil {
				t.Fatalf("parseRolloutView: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseRolloutView mismatch\n got: %+v\nwant: %+v", got, tt.want)
			}
		})
	}
}

// TestRolloutView_StepKinds pins the classification of the non-setWeight/pause step
// kinds and both pause-duration forms (bare seconds and the "30s" string).
func TestRolloutView_StepKinds(t *testing.T) {
	tests := []struct {
		name string
		step string
		want RolloutViewStep
	}{
		{"setWeight zero is still present", `{"setWeight":0}`, RolloutViewStep{Kind: "setWeight", Weight: ptr(0)}},
		{"indefinite pause", `{"pause":{}}`, RolloutViewStep{Kind: "pause", PauseDuration: ""}},
		{"timed pause seconds", `{"pause":{"duration":30}}`, RolloutViewStep{Kind: "pause", PauseDuration: "30"}},
		{"timed pause string", `{"pause":{"duration":"5m"}}`, RolloutViewStep{Kind: "pause", PauseDuration: "5m"}},
		{"analysis", `{"analysis":{"templates":[{"templateName":"t"}]}}`, RolloutViewStep{Kind: "analysis"}},
		{"experiment", `{"experiment":{"templates":[]}}`, RolloutViewStep{Kind: "experiment"}},
		{"setCanaryScale", `{"setCanaryScale":{"replicas":3}}`, RolloutViewStep{Kind: "setCanaryScale"}},
		{"plugin", `{"plugin":{"name":"x"}}`, RolloutViewStep{Kind: "plugin"}},
		{"unknown shape -> other", `{"future":true}`, RolloutViewStep{Kind: "other"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseRolloutViewStep([]byte(tt.step))
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseRolloutViewStep(%s) = %+v, want %+v", tt.step, got, tt.want)
			}
		})
	}
}

// TestRolloutView_ListRollouts exercises the list transport + service end-to-end
// against a RolloutList fixture: the path built, the delegation, and per-item parse.
func TestRolloutView_ListRollouts(t *testing.T) {
	const list = `{"items":[` +
		`{"metadata":{"namespace":"prod","name":"a"},"spec":{"strategy":{"canary":{"steps":[{"setWeight":50}]}}},"status":{"phase":"Progressing","currentStepIndex":0}},` +
		`{"metadata":{"namespace":"prod","name":"b"},"spec":{"strategy":{"blueGreen":{}}},"status":{"phase":"Healthy"}}` +
		`]}`
	g := &fakeGetter{body: []byte(list)}
	proj := uuid.New()
	l := NewRolloutLister(g)

	views, err := l.ListRollouts(context.Background(), "prod-cluster", proj, "prod")
	if err != nil {
		t.Fatalf("ListRollouts: %v", err)
	}
	if g.gotPath != "/apis/argoproj.io/v1alpha1/namespaces/prod/rollouts" {
		t.Errorf("list path = %q", g.gotPath)
	}
	if g.gotCluster != "prod-cluster" || g.gotProject != proj {
		t.Errorf("delegated cluster=%q project=%v", g.gotCluster, g.gotProject)
	}
	if len(views) != 2 {
		t.Fatalf("len(views) = %d, want 2", len(views))
	}
	if views[0].Name != "a" || views[0].Strategy != "canary" || !views[0].CurrentStepKnown {
		t.Errorf("view[0] = %+v", views[0])
	}
	if views[1].Name != "b" || views[1].Strategy != "blueGreen" {
		t.Errorf("view[1] = %+v", views[1])
	}

	// namespace is REQUIRED — an incomplete target fails closed (no collection LIST).
	if _, err := l.ListRollouts(context.Background(), "prod-cluster", proj, ""); err == nil {
		t.Error("want error on empty namespace, got nil")
	}
}

// Cluster-supplied free text (status message, analysis message) is bounded so a giant
// status can't bloat the dashboard payload (mirrors the watch snapshot's cap).
func TestRolloutView_BoundsClusterText(t *testing.T) {
	long := strings.Repeat("x", rolloutViewTextMax+400)
	raw := []byte(`{
		"metadata": {"namespace":"ns","name":"ro"},
		"spec": {"strategy":{"canary":{"steps":[]}}},
		"status": {
			"phase":"Degraded",
			"message":"` + long + `",
			"canary":{"currentStepAnalysisRunStatus":{"name":"a","status":"Failed","message":"` + long + `"}}
		}
	}`)
	v, err := parseRolloutView(raw)
	if err != nil {
		t.Fatalf("parseRolloutView: %v", err)
	}
	if n := len([]rune(v.Message)); n != rolloutViewTextMax {
		t.Fatalf("message runes = %d, want %d (bounded)", n, rolloutViewTextMax)
	}
	if v.Analysis == nil {
		t.Fatal("analysis is nil, want a bounded message")
	}
	if n := len([]rune(v.Analysis.Message)); n != rolloutViewTextMax {
		t.Fatalf("analysis message runes = %d, want %d (bounded)", n, rolloutViewTextMax)
	}
}
