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
}
