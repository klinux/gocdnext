package runner

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
)

// maxTestReportsBytes caps the total payload the agent ships back
// for one job so a runaway test (multi-megabyte stack traces
// concatenated across thousands of cases) doesn't blow the gRPC
// frame. 4 MiB is generous — real-world JUnit reports for
// ~10k cases top out well under that.
const maxTestReportsBytes = 4 << 20

// maxCaseFieldBytes is the per-field clamp applied before sending.
// Mirrors the server's ingest-side clamp; doubling it here means
// the wire carries at most 64 KiB (server truncates again, so
// even a compromised agent can't force oversized rows).
const maxCaseFieldBytes = 64 << 10

// junitSuites is the tolerant top-level shape we decode. Real-
// world reports show up as either <testsuites> with one or more
// <testsuite> children, or a single root <testsuite>. Encoding/xml
// happily decodes both if we declare the aggregate root with
// inline testsuite entries — when the file is a bare <testsuite>
// the unmarshal fills Suites with exactly one element via the
// fallback path below.
type junitSuites struct {
	XMLName xml.Name      `xml:"testsuites"`
	Suites  []junitSuite  `xml:"testsuite"`
}

type junitSuite struct {
	Name    string      `xml:"name,attr"`
	Cases   []junitCase `xml:"testcase"`
}

type junitCase struct {
	Name      string    `xml:"name,attr"`
	Classname string    `xml:"classname,attr"`
	// Time is seconds as a string because JUnit writers sometimes
	// emit "0.123", sometimes "123" (ms), sometimes empty. Parse
	// as seconds with a float fallback; ambiguous units are the
	// cost of a format with no real spec.
	Time      string    `xml:"time,attr"`
	Failure   *junitFail `xml:"failure"`
	Errorr    *junitFail `xml:"error"`
	Skipped   *junitSkip `xml:"skipped"`
	SystemOut string    `xml:"system-out"`
	SystemErr string    `xml:"system-err"`
}

type junitFail struct {
	Type    string `xml:"type,attr"`
	Message string `xml:"message,attr"`
	Detail  string `xml:",chardata"`
}

type junitSkip struct {
	Message string `xml:"message,attr"`
}

// scanTestReports resolves every glob in the assignment's
// TestReports, parses each match as JUnit XML, and ships one
// TestResultBatch back to the server. Errors never fail the job;
// tests are an observability layer, not part of the build
// contract.
func (r *Runner) scanTestReports(ctx context.Context, workDir string, a *gocdnextv1.JobAssignment, seq *atomic.Int64) {
	globs := a.GetTestReports()
	if len(globs) == 0 {
		return
	}

	matches, err := expandGlobs(workDir, globs)
	if err != nil {
		r.emitLog(a, seq, "stderr", fmt.Sprintf("test_reports: glob error: %v", err))
		return
	}
	if len(matches) == 0 {
		// Missing files are a silent no-op — the job may have
		// legitimately produced nothing (tests skipped entirely,
		// suite filtered out).
		return
	}

	results, warnings := parseJUnitFiles(matches)
	for _, w := range warnings {
		r.emitLog(a, seq, "stderr", "test_reports: "+w)
	}
	if len(results) == 0 {
		return
	}

	batch := &gocdnextv1.TestResultBatch{
		RunId:   a.GetRunId(),
		JobId:   a.GetJobId(),
		Results: clampBatch(results),
	}
	r.cfg.Send(&gocdnextv1.AgentMessage{
		Kind: &gocdnextv1.AgentMessage_TestResults{TestResults: batch},
	})
	r.emitLog(a, seq, "stdout",
		fmt.Sprintf("test_reports: shipped %d cases across %d file(s)",
			len(batch.Results), len(matches)))
}

// expandGlobs resolves every pattern relative to workDir, dedupes
// by absolute path, and drops non-files (dirs matching a bare
// glob would confuse the XML parser). Patterns that don't match
// anything are silently skipped — same posture as CI tools that
// treat an absent report as "no tests ran", not an error.
func expandGlobs(workDir string, patterns []string) ([]string, error) {
	seen := map[string]bool{}
	out := []string{}
	for _, pat := range patterns {
		if pat == "" {
			continue
		}
		full := filepath.Join(workDir, pat)
		matches, err := filepath.Glob(full)
		if err != nil {
			return nil, fmt.Errorf("glob %q: %w", pat, err)
		}
		for _, m := range matches {
			if seen[m] {
				continue
			}
			info, err := os.Stat(m)
			if err != nil || info.IsDir() {
				continue
			}
			seen[m] = true
			out = append(out, m)
		}
	}
	return out, nil
}

// parseJUnitFiles parses each file and returns the flat case list
// plus any warnings encountered along the way. Malformed files
// contribute warnings (surfaced as stderr log lines) but never
// abort — one bad file shouldn't swallow results from the others.
func parseJUnitFiles(paths []string) ([]*gocdnextv1.TestResult, []string) {
	var results []*gocdnextv1.TestResult
	var warnings []string
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			warnings = append(warnings, fmt.Sprintf("read %s: %v", p, err))
			continue
		}
		suites, err := decodeJUnit(raw)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("parse %s: %v", p, err))
			continue
		}
		for _, s := range suites {
			for _, c := range s.Cases {
				results = append(results, caseToProto(s.Name, c))
			}
		}
	}
	return results, warnings
}

// decodeJUnit accepts both `<testsuites>` roots and a bare
// `<testsuite>` root. encoding/xml is lenient about element
// names unless we peek at the root ourselves — without that
// check, garbage like `<nope/>` decodes into an empty struct
// with no error.
func decodeJUnit(data []byte) ([]junitSuite, error) {
	root, err := rootElementName(data)
	if err != nil {
		return nil, err
	}
	switch root {
	case "testsuites":
		var agg junitSuites
		if err := xml.Unmarshal(data, &agg); err != nil {
			return nil, fmt.Errorf("testsuites: %w", err)
		}
		return agg.Suites, nil
	case "testsuite":
		var single junitSuite
		if err := xml.Unmarshal(data, &single); err != nil {
			return nil, fmt.Errorf("testsuite: %w", err)
		}
		return []junitSuite{single}, nil
	default:
		return nil, fmt.Errorf("root element %q isn't testsuites or testsuite", root)
	}
}

// rootElementName pulls the first StartElement's local name out
// of the byte slice. Cheaper than a full decode + gives the
// root-vs-unknown distinction we need above.
func rootElementName(data []byte) (string, error) {
	dec := xml.NewDecoder(strings.NewReader(string(data)))
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", fmt.Errorf("no root element: %w", err)
		}
		if se, ok := tok.(xml.StartElement); ok {
			return se.Name.Local, nil
		}
	}
}

func caseToProto(suiteName string, c junitCase) *gocdnextv1.TestResult {
	status := "passed"
	var fType, fMsg, fDetail string
	switch {
	case c.Failure != nil:
		status = "failed"
		fType = c.Failure.Type
		fMsg = c.Failure.Message
		fDetail = strings.TrimSpace(c.Failure.Detail)
	case c.Errorr != nil:
		status = "errored"
		fType = c.Errorr.Type
		fMsg = c.Errorr.Message
		fDetail = strings.TrimSpace(c.Errorr.Detail)
	case c.Skipped != nil:
		status = "skipped"
		fMsg = c.Skipped.Message
	}
	return &gocdnextv1.TestResult{
		Suite:          suiteName,
		Classname:      c.Classname,
		Name:           c.Name,
		Status:         status,
		DurationMillis: parseDurationSeconds(c.Time),
		FailureType:    clampField(fType),
		FailureMessage: clampField(fMsg),
		FailureDetail:  clampField(fDetail),
		SystemOut:      clampField(strings.TrimSpace(c.SystemOut)),
		SystemErr:      clampField(strings.TrimSpace(c.SystemErr)),
	}
}

// parseDurationSeconds turns a JUnit "time" attribute (seconds,
// possibly fractional) into whole milliseconds. Empty, unparseable
// or negative values collapse to zero — the UI renders "—" in
// that case, which is honest about the missing data.
func parseDurationSeconds(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	secs, err := time.ParseDuration(s + "s")
	if err != nil || secs < 0 {
		return 0
	}
	return secs.Milliseconds()
}

func clampField(s string) string {
	if len(s) <= maxCaseFieldBytes {
		return s
	}
	return s[:maxCaseFieldBytes]
}

// clampBatch caps the total wire size of one batch by dropping
// successive cases once the cumulative field bytes exceeds the
// ceiling. The first N cases always survive — partial coverage
// beats "no data because one case blew the budget".
func clampBatch(results []*gocdnextv1.TestResult) []*gocdnextv1.TestResult {
	total := 0
	cut := len(results)
	for i, r := range results {
		total += len(r.Suite) + len(r.Classname) + len(r.Name) +
			len(r.Status) + len(r.FailureType) + len(r.FailureMessage) +
			len(r.FailureDetail) + len(r.SystemOut) + len(r.SystemErr)
		if total > maxTestReportsBytes {
			cut = i
			break
		}
	}
	return results[:cut]
}
