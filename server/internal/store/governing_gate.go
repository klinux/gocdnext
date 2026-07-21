package store

import (
	"encoding/json"
	"fmt"
)

// GoverningGate is the approval-gate config carried on a deploy target
// (deploy_targets.governing_gate JSONB). Its PRESENCE (a non-nil pointer) puts the
// target in CONTROL mode: a rollout-aware canary that pauses indefinitely arms this
// gate on the in-flight deploy; approve -> Promote, reject -> Abort. Absent => the
// target is observe-only (or non-rollout).
//
// The votes themselves reuse the hardened job_run_approvals engine keyed on the
// deploy's job_run_id; this struct is only the policy (who may approve, how many).
type GoverningGate struct {
	// Approvers are allow-listed identities (username or email — matched with the same
	// OIDC name-vs-email logic the job gate uses). Empty + no groups => any authenticated
	// approver counts (quorum-only).
	Approvers []string `json:"approvers,omitempty"`
	// ApproverGroups are group names; membership is expanded at decision time.
	ApproverGroups []string `json:"approver_groups,omitempty"`
	// Required is the quorum — how many distinct approvals promote. >= 1.
	Required int `json:"required"`
	// Description is shown on the approval prompt (what the human is signing off on).
	Description string `json:"description,omitempty"`
}

// maxGateApprovers / maxGateGroups bound the allow-lists so a target can't smuggle an
// unbounded blob into the JSONB column (and, denormalized, onto every watch).
const (
	maxGateApprovers = 100
	maxGateGroups    = 100
	maxGateRequired  = 100
)

// Validate rejects a malformed gate BEFORE it is persisted. Required must be at least
// 1 (a gate that needs zero approvals is not a gate) and is bounded; the allow-lists
// are bounded. A nil gate is valid (no gate) and callers check that separately.
func (g *GoverningGate) Validate() error {
	if g == nil {
		return nil
	}
	if g.Required < 1 {
		return fmt.Errorf("store: governing_gate.required must be >= 1, got %d", g.Required)
	}
	if g.Required > maxGateRequired {
		return fmt.Errorf("store: governing_gate.required must be <= %d, got %d", maxGateRequired, g.Required)
	}
	if len(g.Approvers) > maxGateApprovers {
		return fmt.Errorf("store: governing_gate has too many approvers (%d > %d)", len(g.Approvers), maxGateApprovers)
	}
	if len(g.ApproverGroups) > maxGateGroups {
		return fmt.Errorf("store: governing_gate has too many approver groups (%d > %d)", len(g.ApproverGroups), maxGateGroups)
	}
	return nil
}

// marshalGoverningGate encodes a gate to the JSONB column bytes. A nil gate => nil
// bytes (SQL NULL), which is how "no gate" is stored.
func marshalGoverningGate(g *GoverningGate) ([]byte, error) {
	if g == nil {
		return nil, nil
	}
	b, err := json.Marshal(g)
	if err != nil {
		return nil, fmt.Errorf("store: marshal governing_gate: %w", err)
	}
	return b, nil
}

// unmarshalGoverningGate decodes the JSONB column bytes back to a gate. Empty/NULL
// bytes => nil (no gate). A JSON `null` also decodes to nil.
func unmarshalGoverningGate(b []byte) (*GoverningGate, error) {
	if len(b) == 0 {
		return nil, nil
	}
	var g *GoverningGate
	if err := json.Unmarshal(b, &g); err != nil {
		return nil, fmt.Errorf("store: unmarshal governing_gate: %w", err)
	}
	return g, nil
}

// GoverningGateEqual reports whether two gates are the same config. Used by the
// separation-of-duties check to decide whether a non-admin's upsert would CHANGE the
// gate (a change is admin-only). Order-sensitive on the allow-lists: a reorder is
// treated as a change, which fails closed (a maintainer's UI round-trips the stored
// gate verbatim, so an unchanged resubmit compares equal).
func GoverningGateEqual(a, b *GoverningGate) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if a.Required != b.Required || a.Description != b.Description {
		return false
	}
	return equalStrings(a.Approvers, b.Approvers) && equalStrings(a.ApproverGroups, b.ApproverGroups)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
