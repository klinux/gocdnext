package deploy

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Errors from resolving which Rollout an Application manages. In control mode the
// watcher must fail closed on these (never finalize by Application health); in
// observe-only mode it degrades to Application health.
var (
	// ErrRolloutNotFound: the Application manages no Rollout (or none could be
	// resolved from an explicit rollout_name).
	ErrRolloutNotFound = errors.New("deploy: no Rollout resolved for target")
	// ErrMultipleRollouts: the Application manages more than one Rollout and the
	// target didn't pin one with rollout_name — ambiguous, fail closed.
	ErrMultipleRollouts = errors.New("deploy: multiple Rollouts in application; set rollout_name")
)

const (
	rolloutGroup = "argoproj.io"
	rolloutKind  = "Rollout"
)

// appResources is the minimal slice of an ArgoCD Application's `.status.resources[]`
// used to auto-discover the Rollout the Application manages.
type appResources struct {
	Status struct {
		Resources []struct {
			Group     string `json:"group"`
			Kind      string `json:"kind"`
			Namespace string `json:"namespace"`
			Name      string `json:"name"`
		} `json:"resources"`
	} `json:"status"`
}

// discoverRollout finds the single Rollout the Application manages, matching by
// GROUP + KIND (not kind alone — a same-named kind in another group must not
// match). Zero → ErrRolloutNotFound; more than one → ErrMultipleRollouts.
func discoverRollout(appRaw []byte) (namespace, name string, err error) {
	var app appResources
	if e := json.Unmarshal(appRaw, &app); e != nil {
		return "", "", fmt.Errorf("deploy: decode application resources: %w", e)
	}
	found := 0
	for _, r := range app.Status.Resources {
		if r.Group == rolloutGroup && r.Kind == rolloutKind {
			found++
			namespace, name = r.Namespace, r.Name
		}
	}
	switch {
	case found == 0:
		return "", "", ErrRolloutNotFound
	case found > 1:
		return "", "", ErrMultipleRollouts
	default:
		return namespace, name, nil
	}
}

// rolloutManifest is the minimal decoded slice of a Rollout CR: the canary steps
// (for the count + whether the current step is an indefinite pause) and status.
type rolloutManifest struct {
	Spec struct {
		Strategy struct {
			Canary struct {
				Steps []rolloutStep `json:"steps"`
			} `json:"canary"`
		} `json:"strategy"`
	} `json:"spec"`
	Status struct {
		Phase            string `json:"phase"`
		Message          string `json:"message"`
		CurrentStepIndex int    `json:"currentStepIndex"`
		Abort            bool   `json:"abort"`
		StableRS         string `json:"stableRS"`
		CurrentPodHash   string `json:"currentPodHash"`
		PauseConditions  []struct {
			Reason string `json:"reason"`
		} `json:"pauseConditions"`
	} `json:"status"`
}

// rolloutStep is one canary step. Only `pause` matters here: a nil Pause means the
// step is not a pause (setWeight/analysis/...); a non-nil Pause with an empty
// Duration is an indefinite `pause: {}` (the human-gate step). Duration is
// intstr.IntOrString upstream (int seconds OR "30s"), so it's read as RawMessage and
// only tested for presence.
type rolloutStep struct {
	Pause *struct {
		Duration json.RawMessage `json:"duration"`
	} `json:"pause"`
}

func (s rolloutStep) isIndefinitePause() bool {
	if s.Pause == nil {
		return false
	}
	d := strings.TrimSpace(string(s.Pause.Duration))
	return d == "" || d == "null" || d == `""`
}

// parseRolloutState decodes a Rollout CR into the comparable RolloutState, deriving
// PausedIndefinitely (the human-gate signal) and FullyPromoted (the no-early-finalize
// signal). Tolerates unknown/extra fields (CRD apiVersion drift) by design.
func parseRolloutState(raw []byte) (RolloutState, error) {
	var m rolloutManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return RolloutState{}, fmt.Errorf("deploy: decode rollout: %w", err)
	}
	st := m.Status
	steps := m.Spec.Strategy.Canary.Steps

	pauseReason := ""
	if len(st.PauseConditions) > 0 {
		pauseReason = st.PauseConditions[0].Reason
	}

	// Indefinite canary pause = paused for the CanaryPauseStep reason AND the current
	// step is a pause with no duration.
	pausedIndef := pauseReason == PauseReasonCanaryStep &&
		st.CurrentStepIndex >= 0 && st.CurrentStepIndex < len(steps) &&
		steps[st.CurrentStepIndex].isIndefinitePause()

	// Fully promoted = advanced past all steps AND the new pod hash is the stable one
	// AND healthy. A healthy Application alone is NOT enough (no early finalize).
	fullyPromoted := st.CurrentStepIndex >= len(steps) &&
		st.CurrentPodHash != "" && st.CurrentPodHash == st.StableRS &&
		RolloutPhase(st.Phase) == RolloutHealthy

	return RolloutState{
		Phase:              normalizeRolloutPhase(st.Phase),
		PauseReason:        pauseReason,
		CurrentStepIndex:   st.CurrentStepIndex,
		StepCount:          len(steps),
		Aborted:            st.Abort,
		Message:            st.Message,
		StableHash:         st.StableRS,
		PodHash:            st.CurrentPodHash,
		PausedIndefinitely: pausedIndef,
		FullyPromoted:      fullyPromoted,
	}, nil
}

// normalizeRolloutPhase maps `.status.phase` to a known RolloutPhase, tolerating
// unknown values (returned as-is so the watcher can still surface them).
func normalizeRolloutPhase(s string) RolloutPhase {
	switch RolloutPhase(s) {
	case RolloutProgressing, RolloutPaused, RolloutDegraded, RolloutHealthy:
		return RolloutPhase(s)
	default:
		return RolloutPhase(s)
	}
}
