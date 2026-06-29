package store

import (
	"context"
	"fmt"
)

// FrameworkCoverage is how many of a group's projects are bound to one framework.
type FrameworkCoverage struct {
	Framework string `json:"framework"`
	Covered   int64  `json:"covered"`
}

// ComplianceGroup is the compliance posture for one label-value group: the total
// projects in the group plus per-framework coverage (covered/total = the %).
type ComplianceGroup struct {
	Group         string              `json:"group"`
	ProjectsTotal int64               `json:"projects_total"`
	Frameworks    []FrameworkCoverage `json:"frameworks"`
}

// ComplianceCoverageReport is the posture rollup behind the analytics page's
// compliance section: per label-value group, framework adoption across the
// group's projects. Current state (not time-series), so no window.
type ComplianceCoverageReport struct {
	Key    string            `json:"key"`
	Groups []ComplianceGroup `json:"groups"`
}

// ComplianceCoverage rolls up framework adoption per label-value group for
// labelKey. A live aggregation (low cardinality — projects × a few frameworks).
func (s *Store) ComplianceCoverage(ctx context.Context, labelKey string) (ComplianceCoverageReport, error) {
	totals, err := s.q.ComplianceGroupTotals(ctx, labelKey)
	if err != nil {
		return ComplianceCoverageReport{}, fmt.Errorf("store: compliance group totals: %w", err)
	}
	cov, err := s.q.ComplianceCoverageByFramework(ctx, labelKey)
	if err != nil {
		return ComplianceCoverageReport{}, fmt.Errorf("store: compliance coverage: %w", err)
	}

	idx := make(map[string]int, len(totals))
	groups := make([]ComplianceGroup, 0, len(totals))
	for _, t := range totals {
		idx[t.Grp] = len(groups)
		groups = append(groups, ComplianceGroup{
			Group:         t.Grp,
			ProjectsTotal: t.ProjectsTotal,
			Frameworks:    []FrameworkCoverage{},
		})
	}
	for _, c := range cov {
		if i, ok := idx[c.Grp]; ok {
			groups[i].Frameworks = append(groups[i].Frameworks, FrameworkCoverage{
				Framework: c.Framework,
				Covered:   c.Covered,
			})
		}
	}

	return ComplianceCoverageReport{Key: labelKey, Groups: groups}, nil
}
