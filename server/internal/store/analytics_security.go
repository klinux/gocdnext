package store

import (
	"context"
	"fmt"
)

// SecurityRollupGroup is the open-vulnerability posture for one label-value
// group: per-severity open counts (identity-deduped), the total, the accepted
// count (shown separately), and whether the group has ever been scanned (to tell
// "clean" from "never scanned").
type SecurityRollupGroup struct {
	Group     string `json:"group"`
	HasScans  bool   `json:"has_scans"`
	Critical  int64  `json:"critical"`
	High      int64  `json:"high"`
	Medium    int64  `json:"medium"`
	Low       int64  `json:"low"`
	TotalOpen int64  `json:"total_open"`
	Accepted  int64  `json:"accepted"`
}

// SecurityRollupReport is the org-level security snapshot behind the analytics
// page: open findings per label-value group + org totals. Current state (not a
// time series), so no window.
type SecurityRollupReport struct {
	Key          string                `json:"key"`
	Groups       []SecurityRollupGroup `json:"groups"`
	OrgCritical  int64                 `json:"org_critical"`
	OrgHigh      int64                 `json:"org_high"`
	OrgTotalOpen int64                 `json:"org_total_open"`
	OrgAccepted  int64                 `json:"org_accepted"`
}

// SecurityRollup rolls up open finding identities per label-value group for
// labelKey. Starts from the labeled groups (so clean/zero groups still appear,
// zero-filled, with has_scans set) and LEFT-merges the severity + accepted
// counts. A live aggregation — counts identities from security_finding_states,
// never SARIF occurrences.
func (s *Store) SecurityRollup(ctx context.Context, labelKey string) (SecurityRollupReport, error) {
	groups, err := s.q.SecurityRollupGroups(ctx, labelKey)
	if err != nil {
		return SecurityRollupReport{}, fmt.Errorf("store: security rollup groups: %w", err)
	}
	counts, err := s.q.SecurityRollupCounts(ctx, labelKey)
	if err != nil {
		return SecurityRollupReport{}, fmt.Errorf("store: security rollup counts: %w", err)
	}
	accepted, err := s.q.SecurityRollupAccepted(ctx, labelKey)
	if err != nil {
		return SecurityRollupReport{}, fmt.Errorf("store: security rollup accepted: %w", err)
	}

	idx := make(map[string]int, len(groups))
	out := make([]SecurityRollupGroup, 0, len(groups))
	for _, g := range groups {
		idx[g.Grp] = len(out)
		out = append(out, SecurityRollupGroup{Group: g.Grp, HasScans: g.HasScans})
	}
	for _, c := range counts {
		i, ok := idx[c.Grp]
		if !ok {
			continue
		}
		switch c.Severity {
		case "critical":
			out[i].Critical = c.N
		case "high":
			out[i].High = c.N
		case "medium":
			out[i].Medium = c.N
		case "low":
			out[i].Low = c.N
		}
		out[i].TotalOpen += c.N
	}
	for _, a := range accepted {
		if i, ok := idx[a.Grp]; ok {
			out[i].Accepted = a.N
		}
	}

	report := SecurityRollupReport{Key: labelKey, Groups: out}
	for _, g := range out {
		report.OrgCritical += g.Critical
		report.OrgHigh += g.High
		report.OrgTotalOpen += g.TotalOpen
		report.OrgAccepted += g.Accepted
	}
	return report, nil
}
