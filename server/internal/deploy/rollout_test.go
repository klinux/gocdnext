package deploy

import (
	"errors"
	"testing"
)

func TestParseRolloutState(t *testing.T) {
	// Steps: [setWeight20, pause{}, setWeight60, pause{30s}] — index 1 is the
	// indefinite gate step (matches the lab spike).
	const spec = `"spec":{"strategy":{"canary":{"steps":[{"setWeight":20},{"pause":{}},{"setWeight":60},{"pause":{"duration":"30s"}}]}}}`

	tests := []struct {
		name       string
		raw        string
		wantPhase  RolloutPhase
		wantStep   int
		wantCount  int
		wantIndef  bool
		wantFull   bool
		wantAbort  bool
		wantReason string
	}{
		{
			name:      "paused at indefinite canary step",
			raw:       `{` + spec + `,"status":{"phase":"Paused","currentStepIndex":1,"pauseConditions":[{"reason":"CanaryPauseStep"}],"message":"CanaryPauseStep"}}`,
			wantPhase: RolloutPaused, wantStep: 1, wantCount: 4, wantIndef: true, wantReason: "CanaryPauseStep",
		},
		{
			name:      "paused at a TIMED step is not indefinite",
			raw:       `{` + spec + `,"status":{"phase":"Paused","currentStepIndex":3,"pauseConditions":[{"reason":"CanaryPauseStep"}]}}`,
			wantPhase: RolloutPaused, wantStep: 3, wantCount: 4, wantIndef: false, wantReason: "CanaryPauseStep",
		},
		{
			name:      "blue-green pause is not an indefinite canary pause",
			raw:       `{` + spec + `,"status":{"phase":"Paused","currentStepIndex":1,"pauseConditions":[{"reason":"BlueGreenPause"}]}}`,
			wantPhase: RolloutPaused, wantStep: 1, wantCount: 4, wantIndef: false, wantReason: "BlueGreenPause",
		},
		{
			name:      "fully promoted: past all steps, pod==stable, healthy",
			raw:       `{` + spec + `,"status":{"phase":"Healthy","currentStepIndex":4,"stableRS":"abc","currentPodHash":"abc"}}`,
			wantPhase: RolloutHealthy, wantStep: 4, wantCount: 4, wantFull: true,
		},
		{
			name:      "healthy but pod!=stable is NOT fully promoted (no early finalize)",
			raw:       `{` + spec + `,"status":{"phase":"Healthy","currentStepIndex":4,"stableRS":"abc","currentPodHash":"def"}}`,
			wantPhase: RolloutHealthy, wantStep: 4, wantCount: 4, wantFull: false,
		},
		{
			name:      "aborted + degraded",
			raw:       `{` + spec + `,"status":{"phase":"Degraded","abort":true,"currentStepIndex":1}}`,
			wantPhase: RolloutDegraded, wantStep: 1, wantCount: 4, wantAbort: true,
		},
		{
			name:      "unknown phase tolerated; extra fields ignored",
			raw:       `{` + spec + `,"status":{"phase":"Weird","future":42,"currentStepIndex":0}}`,
			wantPhase: RolloutPhase("Weird"), wantStep: 0, wantCount: 4,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseRolloutState([]byte(tt.raw))
			if err != nil {
				t.Fatalf("parseRolloutState: %v", err)
			}
			if got.Phase != tt.wantPhase || got.CurrentStepIndex != tt.wantStep ||
				got.StepCount != tt.wantCount {
				t.Errorf("phase/step/count = %v/%d/%d, want %v/%d/%d",
					got.Phase, got.CurrentStepIndex, got.StepCount, tt.wantPhase, tt.wantStep, tt.wantCount)
			}
			if got.PausedIndefinitely != tt.wantIndef {
				t.Errorf("PausedIndefinitely = %v, want %v", got.PausedIndefinitely, tt.wantIndef)
			}
			if got.FullyPromoted != tt.wantFull {
				t.Errorf("FullyPromoted = %v, want %v", got.FullyPromoted, tt.wantFull)
			}
			if got.Aborted != tt.wantAbort {
				t.Errorf("Aborted = %v, want %v", got.Aborted, tt.wantAbort)
			}
			if got.PauseReason != tt.wantReason {
				t.Errorf("PauseReason = %q, want %q", got.PauseReason, tt.wantReason)
			}
		})
	}
}

func TestParseRolloutState_malformed(t *testing.T) {
	if _, err := parseRolloutState([]byte(`{not json`)); err == nil {
		t.Fatal("expected a decode error")
	}
}

// An ABSENT currentStepIndex must NOT be trusted as step 0: a CanaryPauseStep with
// step[0]=pause:{} must NOT become PausedIndefinitely (would arm a gate on
// incomplete controller state).
func TestParseRolloutState_absentStepIndexNotIndefinite(t *testing.T) {
	const spec = `"spec":{"strategy":{"canary":{"steps":[{"pause":{}},{"setWeight":50}]}}}`
	raw := `{` + spec + `,"status":{"phase":"Paused","pauseConditions":[{"reason":"CanaryPauseStep"}]}}` // no currentStepIndex
	got, err := parseRolloutState([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if got.CurrentStepKnown {
		t.Error("CurrentStepKnown should be false when the controller didn't report it")
	}
	if got.PausedIndefinitely {
		t.Error("PausedIndefinitely must be false on an unknown step index (no false gate)")
	}
	// A null index behaves the same as absent.
	if g, _ := parseRolloutState([]byte(`{` + spec + `,"status":{"phase":"Paused","currentStepIndex":null,"pauseConditions":[{"reason":"CanaryPauseStep"}]}}`)); g.PausedIndefinitely || g.CurrentStepKnown {
		t.Error("null currentStepIndex must be treated as unknown")
	}
}

func TestDiscoverRollout(t *testing.T) {
	one := `{"status":{"resources":[
		{"group":"","version":"v1","kind":"Service","namespace":"gb","name":"svc"},
		{"group":"argoproj.io","version":"v1alpha1","kind":"Rollout","namespace":"gb","name":"gb-rollout"}
	]}}`
	ns, name, err := discoverRollout([]byte(one))
	if err != nil || ns != "gb" || name != "gb-rollout" {
		t.Fatalf("discover = %q/%q err=%v, want gb/gb-rollout", ns, name, err)
	}

	none := `{"status":{"resources":[{"group":"","kind":"Service","name":"svc"}]}}`
	if _, _, err := discoverRollout([]byte(none)); !errors.Is(err, ErrRolloutNotFound) {
		t.Errorf("no rollout → %v, want ErrRolloutNotFound", err)
	}

	// A same-named kind in another group must NOT match.
	otherGroup := `{"status":{"resources":[{"group":"other.io","kind":"Rollout","namespace":"x","name":"y"}]}}`
	if _, _, err := discoverRollout([]byte(otherGroup)); !errors.Is(err, ErrRolloutNotFound) {
		t.Errorf("other-group Rollout → %v, want ErrRolloutNotFound", err)
	}

	multi := `{"status":{"resources":[
		{"group":"argoproj.io","kind":"Rollout","namespace":"a","name":"r1"},
		{"group":"argoproj.io","kind":"Rollout","namespace":"b","name":"r2"}
	]}}`
	if _, _, err := discoverRollout([]byte(multi)); !errors.Is(err, ErrMultipleRollouts) {
		t.Errorf("multiple → %v, want ErrMultipleRollouts", err)
	}

	// An incomplete entry (missing namespace or name) is not a resolvable target →
	// fail closed as not-found here, not late as a generic fetch error.
	incomplete := `{"status":{"resources":[{"group":"argoproj.io","kind":"Rollout","name":"r1"}]}}`
	if _, _, err := discoverRollout([]byte(incomplete)); !errors.Is(err, ErrRolloutNotFound) {
		t.Errorf("incomplete entry → %v, want ErrRolloutNotFound", err)
	}
}

// The active AnalysisRun is surfaced from the Rollout status' inline {name, status,
// message}, preferring the STEP analysis over BACKGROUND. Observe-only — nothing derives
// gate behavior from it.
func TestParseRolloutState_analysis(t *testing.T) {
	step := `"currentStepAnalysisRunStatus":{"name":"demo-abc-2","status":"Inconclusive","message":"success-rate below threshold"}`
	bg := `"currentBackgroundAnalysisRunStatus":{"name":"demo-bg-1","status":"Running","message":""}`

	tests := []struct {
		name        string
		canary      string
		wantActive  bool
		wantKind    string
		wantName    string
		wantPhase   AnalysisPhase
		wantMessage string
	}{
		{"step analysis", `"canary":{` + step + `}`, true, "step", "demo-abc-2", AnalysisInconclusive, "success-rate below threshold"},
		{"background analysis", `"canary":{` + bg + `}`, true, "background", "demo-bg-1", AnalysisRunning, ""},
		{"step preferred over background", `"canary":{` + step + `,` + bg + `}`, true, "step", "demo-abc-2", AnalysisInconclusive, "success-rate below threshold"},
		{"no analysis", `"canary":{}`, false, "", "", "", ""},
		{"absent canary", ``, false, "", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := `"phase":"Progressing","currentStepIndex":1`
			if tt.canary != "" {
				status += "," + tt.canary
			}
			raw := `{"spec":{"strategy":{"canary":{"steps":[{"setWeight":50},{"analysis":{}}]}}},"status":{` + status + `}}`
			got, err := parseRolloutState([]byte(raw))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got.AnalysisActive != tt.wantActive || got.AnalysisKind != tt.wantKind ||
				got.AnalysisName != tt.wantName || got.AnalysisPhase != tt.wantPhase ||
				got.AnalysisMessage != tt.wantMessage {
				t.Errorf("analysis = {active:%v kind:%q name:%q phase:%q msg:%q}, want {%v %q %q %q %q}",
					got.AnalysisActive, got.AnalysisKind, got.AnalysisName, got.AnalysisPhase, got.AnalysisMessage,
					tt.wantActive, tt.wantKind, tt.wantName, tt.wantPhase, tt.wantMessage)
			}
		})
	}
}

// An analysis phase the CRD reports but we lack a constant for is kept verbatim.
func TestParseRolloutState_unknownAnalysisPhase(t *testing.T) {
	raw := `{"status":{"phase":"Progressing","canary":{"currentStepAnalysisRunStatus":{"name":"x","status":"SomethingNew","message":""}}}}`
	got, _ := parseRolloutState([]byte(raw))
	if got.AnalysisPhase != AnalysisPhase("SomethingNew") {
		t.Errorf("unknown phase = %q, want it kept verbatim", got.AnalysisPhase)
	}
}
