package store

import (
	"strings"
	"testing"
)

func spotAffinityTerm() PreferredNodeAffinityTerm {
	return PreferredNodeAffinityTerm{
		Weight: 100,
		MatchExpressions: []NodeAffinityMatchExpression{
			{Key: "cloud.google.com/gke-spot", Operator: "In", Values: []string{"true"}},
		},
	}
}

func TestValidateAndNormalisePreferredNodeAffinity(t *testing.T) {
	term := func(w int32, e ...NodeAffinityMatchExpression) PreferredNodeAffinityTerm {
		return PreferredNodeAffinityTerm{Weight: w, MatchExpressions: e}
	}
	expr := func(k, op string, v ...string) NodeAffinityMatchExpression {
		return NodeAffinityMatchExpression{Key: k, Operator: op, Values: v}
	}

	tests := []struct {
		name    string
		in      []PreferredNodeAffinityTerm
		wantErr bool
		errSub  string // checked only when wantErr
	}{
		{name: "nil ok", in: nil},
		{name: "valid spot In", in: []PreferredNodeAffinityTerm{spotAffinityTerm()}},
		{name: "valid Exists no values", in: []PreferredNodeAffinityTerm{term(50, expr("k", "Exists"))}},
		{name: "valid Gt integer", in: []PreferredNodeAffinityTerm{term(1, expr("k", "Gt", "3"))}},
		{name: "weight 0", in: []PreferredNodeAffinityTerm{term(0, expr("k", "Exists"))}, wantErr: true, errSub: "weight"},
		{name: "weight 101", in: []PreferredNodeAffinityTerm{term(101, expr("k", "Exists"))}, wantErr: true, errSub: "weight"},
		{name: "empty match_expressions", in: []PreferredNodeAffinityTerm{term(10)}, wantErr: true, errSub: "at least one match_expression"},
		{name: "unknown operator", in: []PreferredNodeAffinityTerm{term(10, expr("k", "Bogus", "x"))}, wantErr: true, errSub: "operator"},
		{name: "In without values", in: []PreferredNodeAffinityTerm{term(10, expr("k", "In"))}, wantErr: true},
		{name: "Exists with values", in: []PreferredNodeAffinityTerm{term(10, expr("k", "Exists", "x"))}, wantErr: true},
		{name: "Gt non-integer", in: []PreferredNodeAffinityTerm{term(10, expr("k", "Gt", "abc"))}, wantErr: true},
		{name: "Gt two values", in: []PreferredNodeAffinityTerm{term(10, expr("k", "Gt", "1", "2"))}, wantErr: true},
		{name: "bad key", in: []PreferredNodeAffinityTerm{term(10, expr("Bad Key!", "In", "x"))}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := ValidateAndNormalisePreferredNodeAffinity(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (out=%v)", out)
				}
				if tt.errSub != "" && !strings.Contains(err.Error(), tt.errSub) {
					t.Errorf("err %q does not contain %q", err.Error(), tt.errSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestValidatePreferredNodeAffinity_TotalLimits(t *testing.T) {
	// > maxAffinityTerms terms.
	tooMany := make([]PreferredNodeAffinityTerm, maxAffinityTerms+1)
	for i := range tooMany {
		tooMany[i] = spotAffinityTerm()
	}
	if _, err := ValidateAndNormalisePreferredNodeAffinity(tooMany); err == nil || !strings.Contains(err.Error(), "too many terms") {
		t.Errorf("expected too-many-terms error, got %v", err)
	}

	// Total expressions cap: few terms, but many expressions overall.
	exprs := make([]NodeAffinityMatchExpression, maxAffinityExpressions+1)
	for i := range exprs {
		exprs[i] = NodeAffinityMatchExpression{Key: "k", Operator: "Exists"}
	}
	oneFatTerm := []PreferredNodeAffinityTerm{{Weight: 1, MatchExpressions: exprs}}
	if _, err := ValidateAndNormalisePreferredNodeAffinity(oneFatTerm); err == nil || !strings.Contains(err.Error(), "too many match_expressions total") {
		t.Errorf("expected too-many-expressions error, got %v", err)
	}

	// Total values cap: one expression with too many values.
	vals := make([]string, maxAffinityValues+1)
	for i := range vals {
		vals[i] = "v"
	}
	fatValues := []PreferredNodeAffinityTerm{{Weight: 1, MatchExpressions: []NodeAffinityMatchExpression{{Key: "k", Operator: "In", Values: vals}}}}
	if _, err := ValidateAndNormalisePreferredNodeAffinity(fatValues); err == nil || !strings.Contains(err.Error(), "too many values total") {
		t.Errorf("expected too-many-values error, got %v", err)
	}
}

func TestValidatePreferredNodeAffinity_DeepCopiesValues(t *testing.T) {
	in := []PreferredNodeAffinityTerm{{
		Weight:           100,
		MatchExpressions: []NodeAffinityMatchExpression{{Key: "k", Operator: "In", Values: []string{"true"}}},
	}}
	out, err := ValidateAndNormalisePreferredNodeAffinity(in)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	// Mutating the caller's input slice must not corrupt the normalised output.
	in[0].MatchExpressions[0].Values[0] = "MUTATED"
	if got := out[0].MatchExpressions[0].Values[0]; got != "true" {
		t.Errorf("output aliased input: got %q, want %q", got, "true")
	}
}
