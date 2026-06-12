// Package runner — coverage.go parses the job's declared coverage
// file (go-cover | lcov | cobertura) after tasks succeed and ships
// ONLY the summary back on the agent stream (CoverageSummary). The
// raw file never crosses the control plane — operators that want it
// declare it under artifacts.optional.
package runner

import (
	"bufio"
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
)

// maxCoverageFileBytes caps the coverage file read. A go-cover
// profile for a large monorepo runs single-digit MiB; 32 MiB is
// generous headroom before we're plainly parsing something that
// isn't a coverage file.
const maxCoverageFileBytes = 32 << 20

// maxCoveragePackages caps the per-package breakdown that rides the
// control stream — the SUMMARY must stay small. Truncation is loud
// (warning into the job log); totals are computed before the cap so
// they stay exact.
const maxCoveragePackages = 200

// scanCoverage is the shared-mode hook: read the declared file from
// the workspace and ship the parsed summary. Returns the fail_under
// gate verdict for the SUCCESS path to act on (failure paths ignore
// it — the job already failed).
//
// With NO gate, a missing/oversized/unreadable file logs an error
// and reports nothing. With fail_under > 0 those same conditions
// FAIL the gate: a gate that silently passes when the profile is
// deleted or corrupted is a bypass, not a gate.
func (r *Runner) scanCoverage(workDir string, a *gocdnextv1.JobAssignment, seq *atomic.Int64) (bool, string) {
	spec := a.GetCoverageReport()
	if spec == nil {
		return false, ""
	}
	gated := spec.GetFailUnder() > 0
	full := filepath.Join(workDir, filepath.FromSlash(spec.GetPath()))
	info, err := os.Stat(full)
	if err != nil {
		msg := fmt.Sprintf("coverage_report: %s not found — the job declared coverage output it didn't produce", spec.GetPath())
		r.emitLog(a, seq, "stderr", msg)
		if gated {
			return true, msg + " (fail_under gate cannot be evaluated)"
		}
		return false, ""
	}
	if info.Size() > maxCoverageFileBytes {
		msg := fmt.Sprintf("coverage_report: %s is %d bytes (cap %d) — skipping", spec.GetPath(), info.Size(), maxCoverageFileBytes)
		r.emitLog(a, seq, "stderr", msg)
		if gated {
			return true, msg + " (fail_under gate cannot be evaluated)"
		}
		return false, ""
	}
	data, err := os.ReadFile(full)
	if err != nil {
		msg := fmt.Sprintf("coverage_report: read %s: %v", spec.GetPath(), err)
		r.emitLog(a, seq, "stderr", msg)
		if gated {
			return true, msg + " (fail_under gate cannot be evaluated)"
		}
		return false, ""
	}
	return r.parseAndSendCoverage(spec, data, a, seq)
}

// parseAndSendCoverage is the mode-agnostic tail shared with the
// isolated-pod variant. Returns the fail_under verdict.
func (r *Runner) parseAndSendCoverage(spec *gocdnextv1.CoverageReportSpec, data []byte, a *gocdnextv1.JobAssignment, seq *atomic.Int64) (bool, string) {
	sum, warns, err := parseCoverage(spec.GetFormat(), data)
	for _, w := range warns {
		r.emitLog(a, seq, "stderr", "coverage_report: "+w)
	}
	if err != nil {
		msg := fmt.Sprintf("coverage_report: parse %s as %s: %v",
			spec.GetPath(), spec.GetFormat(), err)
		r.emitLog(a, seq, "stderr", msg)
		if spec.GetFailUnder() > 0 {
			return true, msg + " (fail_under gate cannot be evaluated)"
		}
		return false, ""
	}
	sum.RunId = a.GetRunId()
	sum.JobId = a.GetJobId()
	sum.Format = spec.GetFormat()
	r.cfg.Send(&gocdnextv1.AgentMessage{
		Kind: &gocdnextv1.AgentMessage_Coverage{Coverage: sum},
	})
	pct := 0.0
	if sum.LinesTotal > 0 {
		pct = 100 * float64(sum.LinesCovered) / float64(sum.LinesTotal)
	}
	r.emitLog(a, seq, "stdout", fmt.Sprintf(
		"coverage_report: %.1f%% (%d/%d lines, %d package(s))",
		pct, sum.LinesCovered, sum.LinesTotal, len(sum.Packages)))
	if failed, reason := coverageGate(spec.GetFailUnder(), sum.LinesCovered, sum.LinesTotal); failed {
		r.emitLog(a, seq, "stderr", "coverage_report: "+reason)
		return true, reason
	}
	return false, ""
}

// coverageGate decides the fail_under verdict. Zero threshold =
// no gate. An empty total with a gate set fails — "no measurable
// lines" cannot satisfy a declared minimum. At-threshold passes
// (the operator wrote "fail UNDER X").
func coverageGate(failUnder float64, covered, total int64) (bool, string) {
	if failUnder <= 0 {
		return false, ""
	}
	if total <= 0 {
		return true, fmt.Sprintf("fail_under %.1f set but the report has no measurable lines", failUnder)
	}
	pct := 100 * float64(covered) / float64(total)
	if pct < failUnder {
		return true, fmt.Sprintf("coverage %.1f%% is below fail_under %.1f%%", pct, failUnder)
	}
	return false, ""
}

// parseCoverage dispatches on format and returns a summary whose
// RunId/JobId/Format the caller stamps. Errors mean "this is not a
// parsable <format> file"; warnings are recoverable oddities.
func parseCoverage(format string, data []byte) (*gocdnextv1.CoverageSummary, []string, error) {
	switch format {
	case "go-cover":
		return parseGoCover(data)
	case "lcov":
		return parseLCOV(data)
	case "cobertura":
		return parseCobertura(data)
	default:
		return nil, nil, fmt.Errorf("unknown format %q", format)
	}
}

// parseGoCover reads `go test -coverprofile` output. Unit is
// STATEMENTS (what `go tool cover -func` reports as %): each block
// line carries its statement count; a block is covered when its
// count > 0. Duplicate blocks (one emission per package run in
// `./...` profiles) merge: counted once, covered if ANY occurrence
// ran.
func parseGoCover(data []byte) (*gocdnextv1.CoverageSummary, []string, error) {
	type block struct {
		stmts   int64
		covered bool
	}
	blocks := map[string]*block{} // "file:range" → block
	pkgOf := map[string]string{}  // blockKey → package dir
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	first := true
	lines := 0
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if first {
			first = false
			if !strings.HasPrefix(line, "mode:") {
				return nil, nil, fmt.Errorf("missing `mode:` header — not a go coverage profile")
			}
			continue
		}
		lines++
		// file.go:SL.SC,EL.EC numStmts count
		lastSpace := strings.LastIndexByte(line, ' ')
		if lastSpace < 0 {
			return nil, nil, fmt.Errorf("malformed profile line %d", lines)
		}
		rest, countStr := line[:lastSpace], line[lastSpace+1:]
		midSpace := strings.LastIndexByte(rest, ' ')
		if midSpace < 0 {
			return nil, nil, fmt.Errorf("malformed profile line %d", lines)
		}
		key, stmtStr := rest[:midSpace], rest[midSpace+1:]
		stmts, err1 := strconv.ParseInt(stmtStr, 10, 64)
		count, err2 := strconv.ParseInt(countStr, 10, 64)
		if err1 != nil || err2 != nil || stmts < 0 {
			return nil, nil, fmt.Errorf("malformed profile line %d", lines)
		}
		b, ok := blocks[key]
		if !ok {
			b = &block{stmts: stmts}
			blocks[key] = b
			file := key
			if i := strings.LastIndexByte(key, ':'); i > 0 {
				file = key[:i]
			}
			pkgOf[key] = path.Dir(file)
		}
		if count > 0 {
			b.covered = true
		}
	}
	if err := sc.Err(); err != nil {
		return nil, nil, err
	}
	if len(blocks) == 0 {
		return nil, nil, fmt.Errorf("profile has no coverage blocks")
	}

	perPkg := map[string]*gocdnextv1.PackageCoverage{}
	sum := &gocdnextv1.CoverageSummary{}
	for key, b := range blocks {
		sum.LinesTotal += b.stmts
		p := pkgOf[key]
		pc := perPkg[p]
		if pc == nil {
			pc = &gocdnextv1.PackageCoverage{Name: p}
			perPkg[p] = pc
		}
		pc.LinesTotal += b.stmts
		if b.covered {
			sum.LinesCovered += b.stmts
			pc.LinesCovered += b.stmts
		}
	}
	pkgs, warns := capPackages(perPkg)
	sum.Packages = pkgs
	return sum, warns, nil
}

// parseLCOV reads lcov tracefiles (vitest/jest/nyc/genhtml input).
// Per record: prefer the LH/LF summary lines; fall back to counting
// DA entries when a tool omits them.
func parseLCOV(data []byte) (*gocdnextv1.CoverageSummary, []string, error) {
	sum := &gocdnextv1.CoverageSummary{}
	perPkg := map[string]*gocdnextv1.PackageCoverage{}
	var (
		curFile          string
		lf, lh           int64
		daTotal, daCover int64
		sawLF            bool
		records          int
		recErr           error
	)
	flush := func() {
		if curFile == "" || recErr != nil {
			return
		}
		records++
		total, covered := daTotal, daCover
		if sawLF {
			total, covered = lf, lh
		}
		// A record claiming more covered than instrumented lines is
		// not a unit disagreement — it's a malformed tracefile, and
		// persisting it paints a >100% package (review-round LOW).
		if total < 0 || covered < 0 || covered > total {
			recErr = fmt.Errorf("record %s: inconsistent LH/LF (%d/%d)", curFile, covered, total)
			return
		}
		sum.LinesTotal += total
		sum.LinesCovered += covered
		p := path.Dir(curFile)
		pc := perPkg[p]
		if pc == nil {
			pc = &gocdnextv1.PackageCoverage{Name: p}
			perPkg[p] = pc
		}
		pc.LinesTotal += total
		pc.LinesCovered += covered
		curFile, lf, lh, daTotal, daCover, sawLF = "", 0, 0, 0, 0, false
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "SF:"):
			flush()
			curFile = strings.TrimPrefix(line, "SF:")
		case strings.HasPrefix(line, "LF:"):
			v, err := strconv.ParseInt(strings.TrimPrefix(line, "LF:"), 10, 64)
			if err != nil && recErr == nil {
				recErr = fmt.Errorf("record %s: unparsable LF %q", curFile, line)
			}
			lf, sawLF = v, true
		case strings.HasPrefix(line, "LH:"):
			v, err := strconv.ParseInt(strings.TrimPrefix(line, "LH:"), 10, 64)
			if err != nil && recErr == nil {
				recErr = fmt.Errorf("record %s: unparsable LH %q", curFile, line)
			}
			lh, sawLF = v, true
		case strings.HasPrefix(line, "DA:"):
			daTotal++
			parts := strings.SplitN(strings.TrimPrefix(line, "DA:"), ",", 3)
			if len(parts) >= 2 {
				if hits, err := strconv.ParseInt(parts[1], 10, 64); err == nil && hits > 0 {
					daCover++
				}
			}
		case line == "end_of_record":
			flush()
		}
	}
	if err := sc.Err(); err != nil {
		return nil, nil, err
	}
	flush()
	if recErr != nil {
		return nil, nil, recErr
	}
	if records == 0 {
		return nil, nil, fmt.Errorf("no SF records — not an lcov tracefile")
	}
	pkgs, warns := capPackages(perPkg)
	sum.Packages = pkgs
	return sum, warns, nil
}

// coberturaXML is the subset of the cobertura DTD we consume. Counts
// come from summing <line> elements (exact), not the float rate
// attributes (lossy).
type coberturaXML struct {
	Packages []struct {
		Name    string `xml:"name,attr"`
		Classes []struct {
			Lines []struct {
				Hits int64 `xml:"hits,attr"`
			} `xml:"lines>line"`
		} `xml:"classes>class"`
	} `xml:"packages>package"`
}

func parseCobertura(data []byte) (*gocdnextv1.CoverageSummary, []string, error) {
	var doc coberturaXML
	dec := xml.NewDecoder(bytes.NewReader(data))
	// The data is already byte-capped by the caller; Strict stays on
	// (default) so HTML-ish garbage errors instead of half-parsing.
	if err := dec.Decode(&doc); err != nil {
		return nil, nil, fmt.Errorf("decode cobertura xml: %w", err)
	}
	if len(doc.Packages) == 0 {
		return nil, nil, fmt.Errorf("no <package> elements — not a cobertura report")
	}
	sum := &gocdnextv1.CoverageSummary{}
	perPkg := map[string]*gocdnextv1.PackageCoverage{}
	for _, p := range doc.Packages {
		pc := perPkg[p.Name]
		if pc == nil {
			pc = &gocdnextv1.PackageCoverage{Name: p.Name}
			perPkg[p.Name] = pc
		}
		for _, c := range p.Classes {
			for _, l := range c.Lines {
				sum.LinesTotal++
				pc.LinesTotal++
				if l.Hits > 0 {
					sum.LinesCovered++
					pc.LinesCovered++
				}
			}
		}
	}
	pkgs, warns := capPackages(perPkg)
	sum.Packages = pkgs
	return sum, warns, nil
}

// capPackages orders the breakdown (worst coverage first — that's
// what an operator scans for) and truncates LOUDLY at the cap.
func capPackages(perPkg map[string]*gocdnextv1.PackageCoverage) ([]*gocdnextv1.PackageCoverage, []string) {
	pkgs := make([]*gocdnextv1.PackageCoverage, 0, len(perPkg))
	for _, pc := range perPkg {
		pkgs = append(pkgs, pc)
	}
	sort.Slice(pkgs, func(i, j int) bool {
		ri := ratio(pkgs[i])
		rj := ratio(pkgs[j])
		if ri != rj {
			return ri < rj
		}
		return pkgs[i].Name < pkgs[j].Name
	})
	var warns []string
	if len(pkgs) > maxCoveragePackages {
		warns = append(warns, fmt.Sprintf(
			"package breakdown truncated to %d of %d (totals stay exact)",
			maxCoveragePackages, len(pkgs)))
		pkgs = pkgs[:maxCoveragePackages]
	}
	return pkgs, warns
}

func ratio(p *gocdnextv1.PackageCoverage) float64 {
	if p.LinesTotal == 0 {
		return 1
	}
	return float64(p.LinesCovered) / float64(p.LinesTotal)
}
