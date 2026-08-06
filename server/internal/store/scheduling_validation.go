package store

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
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
//   - Key (when non-empty) validated against k8svalidation.
//     IsQualifiedName — same rules as node-selector keys + the
//     same rules the apiserver applies to taint keys.
//   - Value (when Operator=Equal) validated against k8svalidation.
//     IsValidLabelValue — the apiserver's exact rule for taint
//     values: empty OR 1-63 chars of alphanumeric / `-_.`. Bad
//     charset / oversize values used to slip past store and fail
//     at pod admission hours later.
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
		if t.Key != "" {
			if errs := k8svalidation.IsQualifiedName(t.Key); len(errs) > 0 {
				return nil, fmt.Errorf("tolerations[%d].key %q: %s", i, t.Key, strings.Join(errs, "; "))
			}
		}
		// Operator=Equal taints values match the label value
		// regex at the apiserver. Operator=Exists already required
		// empty value above, so this check applies only to Equal.
		if t.Operator == "Equal" {
			if errs := k8svalidation.IsValidLabelValue(t.Value); len(errs) > 0 {
				return nil, fmt.Errorf("tolerations[%d].value %q: %s", i, t.Value, strings.Join(errs, "; "))
			}
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

// Preferred node-affinity limits. Bounded so the JobAssignment (shipped over
// gRPC and staged into a Secret) stays small and the scheduler stays cheap.
// Enforced on BOTH the admin API and the YAML seed via applySchedulingValidation.
// Totals are checked in addition to per-term counts so the product of the
// per-dimension caps can't still produce a huge assignment.
const (
	maxAffinityTerms       = 10  // preferred_node_affinity length
	maxAffinityExpressions = 20  // total match_expressions across all terms
	maxAffinityValues      = 100 // total values across all expressions
	maxAffinityWeight      = 100
)

// affinityOperators maps the wire operator string to the k8s selection
// operator so labels.NewRequirement can apply the SAME rules the apiserver
// enforces: key IsQualifiedName; In/NotIn need ≥1 label-valid value;
// Exists/DoesNotExist need zero values; Gt/Lt need exactly one integer. A bad
// expression fails here as a 400 instead of at pod admission hours later.
var affinityOperators = map[string]selection.Operator{
	"In":           selection.In,
	"NotIn":        selection.NotIn,
	"Exists":       selection.Exists,
	"DoesNotExist": selection.DoesNotExist,
	"Gt":           selection.GreaterThan,
	"Lt":           selection.LessThan,
}

// ValidateAndNormalisePreferredNodeAffinity validates every term/expression
// against the apiserver rules and returns a DEEP-COPIED normalised slice (safe
// to persist without aliasing the caller's input). Empty input → empty output.
// A term with zero match_expressions is rejected (a weight with nothing to
// match is meaningless and k8s would reject the pod).
func ValidateAndNormalisePreferredNodeAffinity(in []PreferredNodeAffinityTerm) ([]PreferredNodeAffinityTerm, error) {
	if len(in) == 0 {
		return in, nil
	}
	if len(in) > maxAffinityTerms {
		return nil, fmt.Errorf("preferred_node_affinity: too many terms (%d > %d)", len(in), maxAffinityTerms)
	}
	totalExpr, totalVals := 0, 0
	out := make([]PreferredNodeAffinityTerm, len(in))
	for i, term := range in {
		if term.Weight < 1 || term.Weight > maxAffinityWeight {
			return nil, fmt.Errorf("preferred_node_affinity[%d].weight %d: must be 1..%d", i, term.Weight, maxAffinityWeight)
		}
		if len(term.MatchExpressions) == 0 {
			return nil, fmt.Errorf("preferred_node_affinity[%d]: at least one match_expression is required", i)
		}
		totalExpr += len(term.MatchExpressions)
		if totalExpr > maxAffinityExpressions {
			return nil, fmt.Errorf("preferred_node_affinity: too many match_expressions total (> %d)", maxAffinityExpressions)
		}
		exprs := make([]NodeAffinityMatchExpression, len(term.MatchExpressions))
		for j, e := range term.MatchExpressions {
			op, ok := affinityOperators[e.Operator]
			if !ok {
				return nil, fmt.Errorf("preferred_node_affinity[%d].match_expressions[%d].operator %q: must be In, NotIn, Exists, DoesNotExist, Gt, or Lt", i, j, e.Operator)
			}
			totalVals += len(e.Values)
			if totalVals > maxAffinityValues {
				return nil, fmt.Errorf("preferred_node_affinity: too many values total (> %d)", maxAffinityValues)
			}
			// labels.NewRequirement applies the apiserver's key/value/operator
			// rules; we discard the requirement itself and keep the raw shape.
			if _, err := labels.NewRequirement(e.Key, op, e.Values); err != nil {
				return nil, fmt.Errorf("preferred_node_affinity[%d].match_expressions[%d]: %w", i, j, err)
			}
			exprs[j] = NodeAffinityMatchExpression{
				Key:      e.Key,
				Operator: e.Operator,
				Values:   append([]string(nil), e.Values...), // deep-copy — never alias the input
			}
		}
		out[i] = PreferredNodeAffinityTerm{Weight: term.Weight, MatchExpressions: exprs}
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
	affinity, err := ValidateAndNormalisePreferredNodeAffinity(in.PreferredNodeAffinity)
	if err != nil {
		return err
	}
	in.PreferredNodeAffinity = affinity
	return nil
}
