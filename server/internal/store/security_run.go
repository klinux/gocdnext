package store

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gocdnext/gocdnext/server/internal/db"
)

// RunSecurityFinding is one identity newly introduced by a run, rendered for the
// run page's "new in this change" view.
type RunSecurityFinding struct {
	ScannerJob   string `json:"scanner_job"`
	MatrixKey    string `json:"matrix_key"`
	Tool         string `json:"tool"`
	RuleID       string `json:"rule_id"`
	Severity     string `json:"severity"`
	Message      string `json:"message"`
	LocationPath string `json:"location_path"`
	LocationLine int    `json:"location_line"`
}

// RunSecurity is a run's security snapshot: open counts (identity-deduped),
// accepted (separate), and — for PR runs with a comparable base — the findings
// new in this change. has_scans tells a clean reconciled run from an unscanned
// one; delta_available + unbaselined_series disambiguate "0 new" from
// "no comparable base" (see the per-series rules below).
type RunSecurity struct {
	HasScans          bool                 `json:"has_scans"`
	DeltaAvailable    bool                 `json:"delta_available"`
	UnbaselinedSeries int                  `json:"unbaselined_series"`
	Critical          int64                `json:"critical"`
	High              int64                `json:"high"`
	Medium            int64                `json:"medium"`
	Low               int64                `json:"low"`
	OpenTotal         int64                `json:"open_total"`
	Accepted          int64                `json:"accepted"`
	NewInChange       []RunSecurityFinding `json:"new_in_change"`
}

type seriesKey struct{ scannerJob, matrixKey string }
type identKey4 struct {
	sk       seriesKey
	tool, fp string
}
type identInfo struct {
	severity, state, ruleID, message, path string
	line                                   int
}

// RunSecuritySummary builds a run's security snapshot. Everything is by finding
// IDENTITY (deduped by scanner_job+matrix_key+tool+fingerprint, worst-severity
// wins within the run), never SARIF occurrences. "new in this change" is only
// computed for PR runs against the base-branch baseline, per series:
//   - not a PR / no base_ref     → delta not applicable (delta_available=false, unbaselined=0)
//   - baseline lookup errored    → delta unknown        (delta_available=false, unbaselined=0)
//   - base read, no comparable series → delta_available=false, unbaselined=len(runSeries)
//   - base has series            → delta_available=true, new computed, unbaselined=run series not in base
//
// Open counts + accepted + has_scans are always returned (even for non-PR runs).
func (s *Store) RunSecuritySummary(ctx context.Context, runID uuid.UUID) (RunSecurity, error) {
	out := RunSecurity{NewInChange: []RunSecurityFinding{}}

	// Reconciled series come from security_scans, so a clean scan still counts.
	seriesRows, err := s.q.RunScanSeries(ctx, pgUUID(runID))
	if err != nil {
		return out, fmt.Errorf("store: run scan series: %w", err)
	}
	out.HasScans = len(seriesRows) > 0
	runSeries := make(map[seriesKey]bool, len(seriesRows))
	for _, r := range seriesRows {
		runSeries[seriesKey{r.ScannerJob, r.MatrixKey}] = true
	}

	// Dedupe the run's occurrences to identities (worst-severity wins).
	frows, err := s.q.FindingsByRun(ctx, pgUUID(runID))
	if err != nil {
		return out, fmt.Errorf("store: findings by run: %w", err)
	}
	idents := make(map[identKey4]identInfo, len(frows))
	for _, f := range frows {
		k := identKey4{seriesKey{f.ScannerJob, f.MatrixKey}, f.Tool, f.Fingerprint}
		cur, ok := idents[k]
		if !ok || severityRank(f.Severity) < severityRank(cur.severity) {
			idents[k] = identInfo{
				severity: f.Severity, state: f.State, ruleID: f.RuleID,
				message: f.Message, path: f.LocationPath, line: int(f.LocationLine),
			}
		}
	}
	for _, info := range idents {
		switch info.state {
		case "open":
			switch info.severity {
			case "critical":
				out.Critical++
			case "high":
				out.High++
			case "medium":
				out.Medium++
			case "low":
				out.Low++
			}
			out.OpenTotal++
		case "accepted":
			out.Accepted++
		}
	}

	// Base-branch delta only applies to PR runs with a base ref. An unknown run
	// (valid uuid, no row) is treated as no-delta, not an error — the endpoint
	// stays lenient like coverage.
	bctx, err := s.q.RunBaseContext(ctx, pgUUID(runID))
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, fmt.Errorf("store: run base context: %w", err)
	}
	if bctx.Cause != "pull_request" || bctx.BaseRef == "" {
		return out, nil // delta not applicable
	}

	// Baseline in one snapshot. A FAILED lookup is not an EMPTY baseline:
	// degrade to totals only (delta_available=false, unbaselined=0).
	brows, berr := s.q.SecurityBaseline(ctx, db.SecurityBaselineParams{
		PipelineID: bctx.PipelineID, ExcludeRun: pgUUID(runID), BaseRef: bctx.BaseRef,
	})
	if berr != nil {
		return out, nil
	}
	baseSeries := make(map[seriesKey]bool)
	baseIdent := make(map[identKey4]bool)
	for _, b := range brows {
		sk := seriesKey{b.ScannerJob, b.MatrixKey}
		baseSeries[sk] = true // clean series (NULL tool) still registers
		if b.Tool != nil && b.Fingerprint != nil {
			baseIdent[identKey4{sk, *b.Tool, *b.Fingerprint}] = true
		}
	}

	for sk := range runSeries {
		if baseSeries[sk] {
			out.DeltaAvailable = true
		} else {
			out.UnbaselinedSeries++
		}
	}
	for k, info := range idents {
		if info.state != "open" && info.state != "accepted" {
			continue // dismissed/false_positive never count as new
		}
		if !baseSeries[k.sk] {
			continue // unbaselined series → reported via unbaselined_series, not new
		}
		if baseIdent[k] {
			continue // already present on the base
		}
		out.NewInChange = append(out.NewInChange, RunSecurityFinding{
			ScannerJob: k.sk.scannerJob, MatrixKey: k.sk.matrixKey, Tool: k.tool,
			RuleID: info.ruleID, Severity: info.severity, Message: info.message,
			LocationPath: info.path, LocationLine: info.line,
		})
	}
	sort.Slice(out.NewInChange, func(i, j int) bool {
		a, b := out.NewInChange[i], out.NewInChange[j]
		if ra, rb := severityRank(a.Severity), severityRank(b.Severity); ra != rb {
			return ra < rb
		}
		if a.Tool != b.Tool {
			return a.Tool < b.Tool
		}
		return a.RuleID < b.RuleID
	})
	return out, nil
}
