package parser

import (
	"fmt"
	"math"
	"strings"

	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// coverageFormats is the closed set the agent knows how to parse, one
// per major ecosystem: go-cover (Go), lcov (JS/TS — vitest/jest/nyc),
// cobertura XML (python coverage.py, .NET coverlet), jacoco XML
// (JVM — Java/Kotlin/Gradle/Maven). Adding a format means adding an
// agent parser — keep the set and the parsers in lockstep.
var coverageFormats = map[string]struct{}{
	"go-cover":  {},
	"lcov":      {},
	"cobertura": {},
	"jacoco":    {},
}

// toCoverageReport validates and lowers the YAML block. Path rules
// mirror when.paths / artifacts: workspace-relative, no traversal.
func toCoverageReport(jobName string, def *CoverageReportDef) (*domain.CoverageReportSpec, error) {
	if def == nil {
		return nil, nil
	}
	if def.Path == "" {
		return nil, fmt.Errorf("job %s: coverage_report.path is required", jobName)
	}
	if strings.HasPrefix(def.Path, "/") {
		return nil, fmt.Errorf("job %s: coverage_report.path %q is absolute — paths are workspace-relative", jobName, def.Path)
	}
	for _, seg := range strings.Split(def.Path, "/") {
		if seg == ".." {
			return nil, fmt.Errorf("job %s: coverage_report.path %q contains a '..' segment", jobName, def.Path)
		}
	}
	if def.Format == "" {
		return nil, fmt.Errorf("job %s: coverage_report.format is required (go-cover | lcov | cobertura | jacoco)", jobName)
	}
	if _, ok := coverageFormats[def.Format]; !ok {
		return nil, fmt.Errorf("job %s: coverage_report.format %q unknown (accepted: go-cover, lcov, cobertura, jacoco)", jobName, def.Format)
	}
	// math.IsNaN first: NaN compares false with EVERYTHING, so the
	// range check below would wave it through and the agent's gate
	// (`pct < failUnder`) would silently never fire — a declared
	// gate that is structurally off (review-round MEDIUM).
	if math.IsNaN(def.FailUnder) || math.IsInf(def.FailUnder, 0) {
		return nil, fmt.Errorf("job %s: coverage_report.fail_under must be a finite number", jobName)
	}
	if def.FailUnder < 0 || def.FailUnder > 100 {
		return nil, fmt.Errorf("job %s: coverage_report.fail_under %v out of range (0-100]", jobName, def.FailUnder)
	}
	return &domain.CoverageReportSpec{Path: def.Path, Format: def.Format, FailUnder: def.FailUnder}, nil
}
