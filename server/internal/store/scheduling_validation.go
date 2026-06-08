package store

import (
	"fmt"
	"strings"

	k8svalidation "k8s.io/apimachinery/pkg/util/validation"
)

// ValidateNodeSelector + ValidateAndNormaliseTolerations live in the
// store so every call site that persists a profile — admin HTTP
// handler, YAML seed loader, future API clients — flows through the
// same gate. The store package is the natural backstop: any caller
// that reaches Insert/Update has to go through here, and a bug or
// new code path that forgets to validate up-front still gets caught.
//
// Errors are intentionally typed as raw `error` rather than a
// sentinel — callers translate them to HTTP 400 (admin handler) or
// startup-abort (seed loader); a structured type would force a
// shared exception spec across very different surfaces.

// ValidateNodeSelector validates every (key, value) pair against
// the same rules the k8s apiserver enforces at pod admission time.
// Delegates to k8svalidation.IsQualifiedName + IsValidLabelValue,
// the upstream functions the apiserver itself uses — anything they
// accept gets accepted at pod admission, anything they reject would
// have been rejected anyway. Catching the error here gives operators
// a fixable 400 instead of a pod-stuck-Pending diagnosed hours later.
func ValidateNodeSelector(ns map[string]string) error {
	for k, v := range ns {
		if errs := k8svalidation.IsQualifiedName(k); len(errs) > 0 {
			return fmt.Errorf("node_selector key %q: %s", k, strings.Join(errs, "; "))
		}
		if errs := k8svalidation.IsValidLabelValue(v); len(errs) > 0 {
			return fmt.Errorf("node_selector[%q]: %s", k, strings.Join(errs, "; "))
		}
	}
	return nil
}

// validTolerationOperator: accepted operators. Empty normalises to
// Equal downstream (matches k8s convention — the explicit form lands
// in the audit + UI so the implicit default is never an invisible
// trap).
var validTolerationOperator = map[string]struct{}{
	"":       {},
	"Equal":  {},
	"Exists": {},
}

// validTolerationEffect: empty (matches all effects) or one of the
// three k8s-defined effects. Anything else rejected.
var validTolerationEffect = map[string]struct{}{
	"":                 {},
	"NoSchedule":       {},
	"PreferNoSchedule": {},
	"NoExecute":        {},
}

// ValidateAndNormaliseTolerations enforces the apiserver-level
// invariants that would otherwise surface as a CreatePod 422 hours
// later, AND normalises empty Operator to "Equal" so downstream
// consumers (engine, audit, UI) only see the explicit form.
//
//   - Operator ∈ {Equal, Exists}; empty normalises to Equal.
//   - Effect ∈ {"", NoSchedule, PreferNoSchedule, NoExecute}.
//   - Operator=Exists with non-empty Value rejected (k8s spec).
//   - Empty Key + Equal rejected as meaningless; empty Key + Exists
//     is legal (kubelet "tolerate-everything" pattern).
//   - TolerationSeconds must be ≥ 0 when set, and only with
//     Effect=NoExecute. k8s silently ignores it elsewhere — we
//     reject loud because silent surprises age badly.
//
// The returned slice is the normalised form; persist it (not the
// input) so consumers see the explicit Operator value.
func ValidateAndNormaliseTolerations(in []Toleration) ([]Toleration, error) {
	if len(in) == 0 {
		return in, nil
	}
	out := make([]Toleration, len(in))
	for i, t := range in {
		if _, ok := validTolerationOperator[t.Operator]; !ok {
			return nil, fmt.Errorf("tolerations[%d].operator %q: must be Equal or Exists", i, t.Operator)
		}
		if _, ok := validTolerationEffect[t.Effect]; !ok {
			return nil, fmt.Errorf("tolerations[%d].effect %q: must be \"\", NoSchedule, PreferNoSchedule, or NoExecute", i, t.Effect)
		}
		if t.Operator == "" {
			t.Operator = "Equal"
		}
		if t.Operator == "Exists" && t.Value != "" {
			return nil, fmt.Errorf("tolerations[%d]: operator=Exists requires empty value (got %q)", i, t.Value)
		}
		if t.Key == "" && t.Operator != "Exists" {
			return nil, fmt.Errorf("tolerations[%d]: key required unless operator=Exists", i)
		}
		if t.TolerationSeconds != nil {
			if *t.TolerationSeconds < 0 {
				return nil, fmt.Errorf("tolerations[%d].toleration_seconds: must be ≥ 0 (got %d)", i, *t.TolerationSeconds)
			}
			if t.Effect != "NoExecute" {
				return nil, fmt.Errorf("tolerations[%d]: toleration_seconds only valid with effect=NoExecute (got %q)", i, t.Effect)
			}
		}
		out[i] = t
	}
	return out, nil
}

// applySchedulingValidation is the internal shim every store
// Insert/Update path runs before persist. Mutates input in place
// (normalised tolerations replace the raw slice; node_selector
// stays as-is, only validated). Returns the first violation; callers
// turn it into the right surface (HTTP 400, startup abort, etc).
func applySchedulingValidation(in *RunnerProfileInput) error {
	if err := ValidateNodeSelector(in.NodeSelector); err != nil {
		return err
	}
	normalised, err := ValidateAndNormaliseTolerations(in.Tolerations)
	if err != nil {
		return err
	}
	in.Tolerations = normalised
	return nil
}
