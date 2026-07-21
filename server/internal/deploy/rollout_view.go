package deploy

import (
	"encoding/json"
	"fmt"
	"strings"
)

// RolloutView is a rich, UI-facing snapshot of one Argo Rollouts Rollout — the read
// model behind the rollouts dashboard (ADR-0001). Unlike RolloutState (which stays
// comparable-by-== so the gate state machine can use it), RolloutView carries the
// canary step list and a nullable analysis pointer, so it is intentionally NOT
// comparable. It mirrors parseRolloutState's nullable-index discipline: an absent
// `.status.currentStepIndex` is reported UNKNOWN, never trusted as step 0.
type RolloutView struct {
	Namespace string
	Name      string
	Strategy  string // "canary" | "blueGreen" ("" when neither strategy is present)

	Phase   RolloutPhase
	Message string
	Aborted bool

	CurrentStepIndex int
	CurrentStepKnown bool
	Steps            []RolloutViewStep

	CanaryWeight int    // `.status.canary.weights.canary.weight` (0 when unreported)
	StableHash   string // `.status.stableRS`
	PodHash      string // `.status.currentPodHash`
	Image        string // `.spec.template.spec.containers[0].image`

	// Blue-green specifics — parsed ONLY when Strategy=="blueGreen"; a canary Rollout
	// leaves them zero/empty. The active/preview pod hashes reuse StableHash (active) and
	// PodHash (preview); the preview image reuses Image.
	ActiveService  string // `.spec.strategy.blueGreen.activeService`  ("" when absent)
	PreviewService string // `.spec.strategy.blueGreen.previewService` ("" when absent)
	// ScaleDownDelaySeconds is `.spec.strategy.blueGreen.scaleDownDelaySeconds`, surfaced
	// as-is. 0 means the field is unset (or an explicit 0) — the controller's default is 30s
	// when absent, which the UI notes; the API does not synthesise the 30 so the CR stays
	// the single source of truth.
	ScaleDownDelaySeconds int

	// Analysis is the inline AnalysisRun the Rollout reports for its current step (or,
	// failing that, the background run). For a blueGreen Rollout — which has no canary
	// steps, so this is never populated by the canary branch — it doubles as the
	// pre-promotion AnalysisRun summary (`.status.blueGreen.prePromotionAnalysisRunStatus`).
	// nil when no analysis is active.
	Analysis *RolloutAnalysis
}

// RolloutViewStep is one canary step, classified for display. Kind ∈
// {setWeight, pause, analysis, experiment, setCanaryScale, plugin, other}. Weight is
// set only for a setWeight step. PauseDuration is meaningful only for a pause step: ""
// is an indefinite `pause: {}` (the human-gate step); otherwise it's the duration
// string ("30s"/"5m"/bare seconds like "10").
type RolloutViewStep struct {
	Kind          string
	Weight        *int
	PauseDuration string
}

// RolloutAnalysis is the inline AnalysisRun status the Rollout `.status` already
// carries for its current analysis. Per-metric detail (each measurement) is OUT OF
// SCOPE here — it needs a separate AnalysisRun CR fetch (a follow-up); this is the
// summary the Rollout reports, enough to show WHY a canary paused/degraded.
type RolloutAnalysis struct {
	Name    string
	Phase   AnalysisPhase
	Message string
}

// rolloutViewDoc is the decode target for parseRolloutView: a superset of
// rolloutManifest that also reads the display-only fields (identity, container image,
// canary weight, blue-green presence). Kept separate from rolloutManifest by design —
// the gate machine's minimal parse (parseRolloutState) must stay untouched.
type rolloutViewDoc struct {
	Metadata struct {
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Strategy struct {
			// Pointer so mere PRESENCE of the canary key (even `canary: {}`) selects the
			// strategy, independent of whether any steps are listed.
			Canary *struct {
				Steps []json.RawMessage `json:"steps"`
			} `json:"canary"`
			// Pointer for the same PRESENCE-selects-strategy reason: `blueGreen: {}` (no
			// fields) still selects blueGreen. The service names + scale-down delay are read
			// straight off it when present.
			BlueGreen *struct {
				ActiveService         string `json:"activeService"`
				PreviewService        string `json:"previewService"`
				ScaleDownDelaySeconds int    `json:"scaleDownDelaySeconds"`
			} `json:"blueGreen"`
		} `json:"strategy"`
		Template struct {
			Spec struct {
				Containers []struct {
					Image string `json:"image"`
				} `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
	Status struct {
		Phase   string `json:"phase"`
		Message string `json:"message"`
		Abort   bool   `json:"abort"`
		// Nullable: absent/null must NOT be read as step 0 (see RolloutState).
		CurrentStepIndex *int   `json:"currentStepIndex"`
		StableRS         string `json:"stableRS"`
		CurrentPodHash   string `json:"currentPodHash"`
		Canary           struct {
			// Nullable: the controller reports weights only once it has computed them.
			Weights *struct {
				Canary struct {
					Weight int `json:"weight"`
				} `json:"canary"`
			} `json:"weights"`
			CurrentStepAnalysisRunStatus       *analysisRunStatus `json:"currentStepAnalysisRunStatus"`
			CurrentBackgroundAnalysisRunStatus *analysisRunStatus `json:"currentBackgroundAnalysisRunStatus"`
		} `json:"canary"`
		BlueGreen struct {
			// The pre-promotion AnalysisRun the controller ran BEFORE swapping the active
			// service — same inline {name,status,message} shape as the canary analysis.
			PrePromotionAnalysisRunStatus *analysisRunStatus `json:"prePromotionAnalysisRunStatus"`
		} `json:"blueGreen"`
	} `json:"status"`
}

// rolloutViewTextMax bounds cluster-supplied free text (the status message and the
// analysis message) so a giant status can't bloat the dashboard payload — mirrors the
// watch snapshot's 500-rune cap (store.rolloutTextMax), applied here at parse time
// because the view is read-and-serve, never persisted.
const rolloutViewTextMax = 500

// truncateRunes caps s to max runes (counting runes, not bytes, so a multi-byte rune
// is never split). Kept local to the deploy package; the store's copy is unexported.
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

// parseRolloutView decodes a Rollout CR into the rich RolloutView. It tolerates
// unknown/extra fields (CRD apiVersion drift) and, like parseRolloutState, treats an
// absent currentStepIndex as UNKNOWN rather than step 0. Cluster-supplied free text is
// bounded to rolloutViewTextMax runes.
func parseRolloutView(raw []byte) (RolloutView, error) {
	var d rolloutViewDoc
	if err := json.Unmarshal(raw, &d); err != nil {
		return RolloutView{}, fmt.Errorf("deploy: decode rollout view: %w", err)
	}

	v := RolloutView{
		Namespace:  d.Metadata.Namespace,
		Name:       d.Metadata.Name,
		Strategy:   strategyOf(d),
		Phase:      normalizeRolloutPhase(d.Status.Phase),
		Message:    truncateRunes(d.Status.Message, rolloutViewTextMax),
		Aborted:    d.Status.Abort,
		StableHash: d.Status.StableRS,
		PodHash:    d.Status.CurrentPodHash,
		Image:      firstContainerImage(d),
	}
	if idx := d.Status.CurrentStepIndex; idx != nil {
		v.CurrentStepKnown = true
		v.CurrentStepIndex = *idx
	}
	if w := d.Status.Canary.Weights; w != nil {
		v.CanaryWeight = w.Canary.Weight
	}
	// Steps come from the canary strategy only; a blue-green Rollout has none.
	if c := d.Spec.Strategy.Canary; c != nil {
		v.Steps = make([]RolloutViewStep, 0, len(c.Steps))
		for _, s := range c.Steps {
			v.Steps = append(v.Steps, parseRolloutViewStep(s))
		}
	}
	// Prefer the STEP analysis (directly gates the current step) over the background one.
	if a := d.Status.Canary.CurrentStepAnalysisRunStatus; a != nil {
		v.Analysis = toRolloutAnalysis(a)
	} else if a := d.Status.Canary.CurrentBackgroundAnalysisRunStatus; a != nil {
		v.Analysis = toRolloutAnalysis(a)
	}
	// Blue-green specifics come from the blueGreen strategy only; a canary Rollout has no
	// blueGreen key so these stay zero/empty.
	if bg := d.Spec.Strategy.BlueGreen; bg != nil {
		v.ActiveService = bg.ActiveService
		v.PreviewService = bg.PreviewService
		v.ScaleDownDelaySeconds = bg.ScaleDownDelaySeconds
		// A blueGreen Rollout has no canary steps, so the canary branch above never set
		// Analysis — reuse the field for the pre-promotion AnalysisRun summary. Guard on
		// nil anyway so a (contract-impossible) canary analysis is never clobbered.
		if v.Analysis == nil {
			if a := d.Status.BlueGreen.PrePromotionAnalysisRunStatus; a != nil {
				v.Analysis = toRolloutAnalysis(a)
			}
		}
	}
	return v, nil
}

func toRolloutAnalysis(a *analysisRunStatus) *RolloutAnalysis {
	return &RolloutAnalysis{
		Name:    a.Name,
		Phase:   AnalysisPhase(a.Status),
		Message: truncateRunes(a.Message, rolloutViewTextMax),
	}
}

// strategyOf reports the Rollout's strategy: "canary" when `.spec.strategy.canary` is
// present, else "blueGreen" when `.spec.strategy.blueGreen` is, else "".
func strategyOf(d rolloutViewDoc) string {
	switch {
	case d.Spec.Strategy.Canary != nil:
		return "canary"
	case d.Spec.Strategy.BlueGreen != nil:
		return "blueGreen"
	default:
		return ""
	}
}

func firstContainerImage(d rolloutViewDoc) string {
	if c := d.Spec.Template.Spec.Containers; len(c) > 0 {
		return c[0].Image
	}
	return ""
}

// parseRolloutViewStep classifies one canary step for display. A well-formed step sets
// exactly one action key; the first recognised one wins, and an unrecognised shape
// falls through to "other" (forward-compatible with new argo-rollouts step types). A
// malformed step degrades to "other" rather than failing the whole list.
func parseRolloutViewStep(raw json.RawMessage) RolloutViewStep {
	var probe struct {
		SetWeight      *int            `json:"setWeight"`
		Pause          json.RawMessage `json:"pause"`
		Analysis       json.RawMessage `json:"analysis"`
		Experiment     json.RawMessage `json:"experiment"`
		SetCanaryScale json.RawMessage `json:"setCanaryScale"`
		Plugin         json.RawMessage `json:"plugin"`
	}
	_ = json.Unmarshal(raw, &probe)

	switch {
	case probe.SetWeight != nil:
		return RolloutViewStep{Kind: "setWeight", Weight: probe.SetWeight}
	case present(probe.Pause):
		step := RolloutViewStep{Kind: "pause"}
		// Reuse the gate machine's classifier: an indefinite pause:{} keeps
		// PauseDuration=="" (the human-gate signal); a timed pause renders its duration.
		var rs rolloutStep
		_ = json.Unmarshal(raw, &rs)
		if rs.Pause != nil && !rs.isIndefinitePause() {
			step.PauseDuration = durationString(rs.Pause.Duration)
		}
		return step
	case present(probe.Analysis):
		return RolloutViewStep{Kind: "analysis"}
	case present(probe.Experiment):
		return RolloutViewStep{Kind: "experiment"}
	case present(probe.SetCanaryScale):
		return RolloutViewStep{Kind: "setCanaryScale"}
	case present(probe.Plugin):
		return RolloutViewStep{Kind: "plugin"}
	default:
		return RolloutViewStep{Kind: "other"}
	}
}

// durationString renders a Rollout pause duration (intstr.IntOrString upstream): a
// JSON string "30s" → "30s"; a bare number 10 (seconds) → "10". Empty/null → "".
func durationString(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	if s[0] == '"' {
		var str string
		if err := json.Unmarshal(raw, &str); err == nil {
			return str
		}
	}
	return s
}

// present reports whether a RawMessage carries a real JSON value (not absent/null).
func present(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "null"
}

// parseRolloutList decodes a k8s RolloutList (`{"items":[<Rollout CR>,...]}`) into rich
// views, parsing each item via parseRolloutView. A single undecodable item fails the
// whole list: a partial list would silently drop Rollouts from the dashboard.
func parseRolloutList(raw []byte) ([]RolloutView, error) {
	var list struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("deploy: decode rollout list: %w", err)
	}
	out := make([]RolloutView, 0, len(list.Items))
	for i, item := range list.Items {
		v, err := parseRolloutView(item)
		if err != nil {
			return nil, fmt.Errorf("deploy: decode rollout list item %d: %w", i, err)
		}
		out = append(out, v)
	}
	return out, nil
}
