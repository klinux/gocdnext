package store

import (
	"encoding/json"
	"testing"
)

func TestResolveEffectiveQuorum_NoMatchKeepsBaseline(t *testing.T) {
	// Operator declared quorum_by_label but the PR's labels don't
	// intersect any key → baseline Required applies, returned
	// label is "" so the materialiser persists approval_quorum_label
	// = NULL.
	cause := "pull_request"
	detail := json.RawMessage(`{"pr_number":1,"pr_labels":["chore","backend"]}`)
	overrides := map[string]int{"hotfix": 1, "risky": 3}

	gotQuorum, gotLabel := resolveEffectiveQuorum(cause, detail, 2, overrides)
	if gotQuorum != 2 {
		t.Errorf("quorum = %d, want 2 (baseline)", gotQuorum)
	}
	if gotLabel != "" {
		t.Errorf("label = %q, want \"\" (no override fired)", gotLabel)
	}
}

func TestResolveEffectiveQuorum_SingleLabelMatch(t *testing.T) {
	// Single PR label matches → that override applies.
	detail := json.RawMessage(`{"pr_labels":["hotfix"]}`)
	gotQuorum, gotLabel := resolveEffectiveQuorum("pull_request", detail, 2,
		map[string]int{"hotfix": 1, "risky": 3})
	if gotQuorum != 1 {
		t.Errorf("quorum = %d, want 1", gotQuorum)
	}
	if gotLabel != "hotfix" {
		t.Errorf("label = %q, want hotfix", gotLabel)
	}
}

func TestResolveEffectiveQuorum_MultipleLabelsMaxWins(t *testing.T) {
	// PR has both `hotfix` (override 1) and `risky` (override 3).
	// MAX wins so the gate stays at 3 — two reasons to demand
	// MORE quorum shouldn't cancel each other.
	detail := json.RawMessage(`{"pr_labels":["hotfix","risky","chore"]}`)
	gotQuorum, gotLabel := resolveEffectiveQuorum("pull_request", detail, 2,
		map[string]int{"hotfix": 1, "risky": 3})
	if gotQuorum != 3 {
		t.Errorf("quorum = %d, want 3 (max wins)", gotQuorum)
	}
	if gotLabel != "risky" {
		t.Errorf("label = %q, want risky", gotLabel)
	}
}

func TestResolveEffectiveQuorum_MaxTiedDeterministicByLabel(t *testing.T) {
	// Tied overrides → lexicographic-ascending label wins, so
	// runs/tests/audit don't flap between equivalent gates.
	detail := json.RawMessage(`{"pr_labels":["zebra","alpha","mike"]}`)
	gotQuorum, gotLabel := resolveEffectiveQuorum("pull_request", detail, 1,
		map[string]int{"alpha": 3, "mike": 3, "zebra": 3})
	if gotQuorum != 3 {
		t.Errorf("quorum = %d, want 3", gotQuorum)
	}
	if gotLabel != "alpha" {
		t.Errorf("label = %q, want alpha (lex-min on tie)", gotLabel)
	}
}

func TestResolveEffectiveQuorum_NonPRCauseSkipsResolver(t *testing.T) {
	// push/tag/manual/upstream — labels are PR-only state, so the
	// resolver short-circuits and returns baseline.
	for _, cause := range []string{"push", "tag", "manual", "upstream", "schedule", "poll"} {
		detail := json.RawMessage(`{"pr_labels":["hotfix"]}`) // would match if resolved
		gotQuorum, gotLabel := resolveEffectiveQuorum(cause, detail, 2,
			map[string]int{"hotfix": 1})
		if gotQuorum != 2 || gotLabel != "" {
			t.Errorf("cause %q: got (%d, %q), want (2, \"\")", cause, gotQuorum, gotLabel)
		}
	}
}

func TestResolveEffectiveQuorum_NoOverridesMapShortCircuits(t *testing.T) {
	// Job has no quorum_by_label (the common case) → resolver
	// returns baseline without decoding cause_detail at all.
	detail := json.RawMessage(`{"pr_labels":["anything"]}`)
	gotQuorum, gotLabel := resolveEffectiveQuorum("pull_request", detail, 2, nil)
	if gotQuorum != 2 || gotLabel != "" {
		t.Errorf("nil overrides: got (%d, %q), want (2, \"\")", gotQuorum, gotLabel)
	}
}

func TestResolveEffectiveQuorum_MalformedCauseDetailKeepsBaseline(t *testing.T) {
	// A cause_detail that doesn't decode as JSON, or decodes but
	// lacks pr_labels — both yield baseline. Failing closed (more
	// approvers needed) is safer than failing open.
	cases := []struct {
		name   string
		detail json.RawMessage
	}{
		{"malformed json", json.RawMessage(`{not-json`)},
		{"missing labels", json.RawMessage(`{"pr_number":1}`)},
		{"labels not array", json.RawMessage(`{"pr_labels":"hotfix"}`)},
		{"empty labels", json.RawMessage(`{"pr_labels":[]}`)},
		{"nil detail", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotQuorum, gotLabel := resolveEffectiveQuorum("pull_request", tc.detail, 2,
				map[string]int{"hotfix": 1})
			if gotQuorum != 2 || gotLabel != "" {
				t.Errorf("got (%d, %q), want (2, \"\")", gotQuorum, gotLabel)
			}
		})
	}
}
