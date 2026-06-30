// Package sarif parses SARIF scanner reports into normalized security findings
// (#71). Pure + dependency-free so it's trivially unit-testable; the server
// reads the SARIF artifact blob and feeds it here. Severity is resolved across
// the result + its rule descriptor because tools (notably semgrep) often omit
// per-result level and carry CVSS/severity on the rule.
package sarif

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// MaxFindings caps how many findings a single SARIF file yields (defence against
// a pathological report). Excess is dropped; the caller logs the truncation.
const MaxFindings = 5000

// String-field caps — external-tool SARIF can be very verbose.
const (
	maxMessageLen = 2000
	maxFieldLen   = 500
)

// Severity buckets (v1 normalization).
const (
	SevCritical = "critical"
	SevHigh     = "high"
	SevMedium   = "medium"
	SevLow      = "low"
)

// Finding is one normalized security finding.
type Finding struct {
	Tool         string
	RuleID       string
	Severity     string // critical|high|medium|low
	Level        string // raw SARIF level (error|warning|note|none), for reference
	Message      string
	LocationPath string
	LocationLine int
	LocationURL  string
	Fingerprint  string
}

// --- SARIF wire shapes (only the fields we read) ---

type doc struct {
	Runs []run `json:"runs"`
}

type run struct {
	Tool    tool     `json:"tool"`
	Results []result `json:"results"`
}

type tool struct {
	Driver driver `json:"driver"`
}

type driver struct {
	Name  string `json:"name"`
	Rules []rule `json:"rules"`
}

type rule struct {
	ID                   string               `json:"id"`
	DefaultConfiguration defaultConfiguration `json:"defaultConfiguration"`
	Properties           map[string]any       `json:"properties"`
}

type defaultConfiguration struct {
	Level string `json:"level"`
}

type result struct {
	RuleID              string            `json:"ruleId"`
	RuleIndex           *int              `json:"ruleIndex"`
	Level               string            `json:"level"`
	Message             message           `json:"message"`
	Locations           []location        `json:"locations"`
	Properties          map[string]any    `json:"properties"`
	PartialFingerprints map[string]string `json:"partialFingerprints"`
	Fingerprints        map[string]string `json:"fingerprints"`
}

type message struct {
	Text string `json:"text"`
}

type location struct {
	PhysicalLocation physicalLocation `json:"physicalLocation"`
}

type physicalLocation struct {
	ArtifactLocation artifactLocation `json:"artifactLocation"`
	Region           region           `json:"region"`
}

type artifactLocation struct {
	URI string `json:"uri"`
}

type region struct {
	StartLine int `json:"startLine"`
}

// Parse reads one SARIF document and returns its normalized findings (capped at
// MaxFindings). Returns an error on invalid JSON. truncated reports whether the
// MaxFindings cap dropped any findings.
func Parse(r io.Reader) (findings []Finding, truncated bool, err error) {
	var d doc
	if err := json.NewDecoder(r).Decode(&d); err != nil {
		return nil, false, fmt.Errorf("sarif: decode: %w", err)
	}
	out := make([]Finding, 0, 64)
	for _, rn := range d.Runs {
		toolName := capField(rn.Tool.Driver.Name)
		rules := indexRules(rn.Tool.Driver.Rules)
		for i := range rn.Results {
			if len(out) >= MaxFindings {
				return out, true, nil
			}
			out = append(out, normalize(toolName, &rn.Results[i], rules))
		}
	}
	return out, false, nil
}

// indexRules maps ruleId → rule descriptor for severity resolution.
func indexRules(rs []rule) map[string]rule {
	m := make(map[string]rule, len(rs))
	for _, r := range rs {
		if r.ID != "" {
			m[r.ID] = r
		}
	}
	return m
}

func normalize(toolName string, res *result, rules map[string]rule) Finding {
	ruleID := res.RuleID
	rl, hasRule := rules[ruleID]

	sev, level := resolveSeverity(res, rl, hasRule)
	path, line, url := firstLocation(res.Locations)

	f := Finding{
		Tool:         toolName,
		RuleID:       capField(ruleID),
		Severity:     sev,
		Level:        level,
		Message:      capStr(strings.TrimSpace(res.Message.Text), maxMessageLen),
		LocationPath: capField(path),
		LocationLine: line,
		LocationURL:  capField(url),
	}
	f.Fingerprint = fingerprint(res, f)
	return f
}

// resolveSeverity returns the normalized severity bucket + the raw level.
// Order: result CVSS → rule CVSS → result.level → rule.defaultConfiguration.level
// → rule.properties.severity (text) → low.
func resolveSeverity(res *result, rl rule, hasRule bool) (severity, level string) {
	level = res.Level
	if level == "" && hasRule {
		level = rl.DefaultConfiguration.Level
	}

	if cvss, ok := cvss(res.Properties); ok {
		return bucketCVSS(cvss), level
	}
	if hasRule {
		if cvss, ok := cvss(rl.Properties); ok {
			return bucketCVSS(cvss), level
		}
	}
	if res.Level != "" {
		return bucketLevel(res.Level), level
	}
	if hasRule && rl.DefaultConfiguration.Level != "" {
		return bucketLevel(rl.DefaultConfiguration.Level), level
	}
	if hasRule {
		if s := textSeverity(rl.Properties); s != "" {
			return s, level
		}
	}
	return SevLow, level
}

// cvss reads a "security-severity" CVSS score (0–10) from a properties bag.
// SARIF carries it as a string; some tools use a number.
func cvss(props map[string]any) (float64, bool) {
	if props == nil {
		return 0, false
	}
	v, ok := props["security-severity"]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		if err != nil {
			return 0, false
		}
		return f, true
	case float64:
		return n, true
	default:
		return 0, false
	}
}

func bucketCVSS(score float64) string {
	switch {
	case score >= 9.0:
		return SevCritical
	case score >= 7.0:
		return SevHigh
	case score >= 4.0:
		return SevMedium
	default:
		return SevLow
	}
}

// bucketLevel maps a SARIF level to a severity. CRITICAL is reserved for CVSS in
// v1, so a bare "error" is high.
func bucketLevel(level string) string {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "error":
		return SevHigh
	case "warning":
		return SevMedium
	default: // note, none, empty
		return SevLow
	}
}

// textSeverity maps a free-text rule severity (e.g. "CRITICAL", "HIGH", "ERROR")
// to a bucket. Returns "" when absent/unrecognized.
func textSeverity(props map[string]any) string {
	if props == nil {
		return ""
	}
	v, ok := props["severity"].(string)
	if !ok {
		return ""
	}
	switch strings.ToUpper(strings.TrimSpace(v)) {
	case "CRITICAL":
		return SevCritical
	case "HIGH", "ERROR":
		return SevHigh
	case "MEDIUM", "MODERATE", "WARNING":
		return SevMedium
	case "LOW", "INFO", "INFORMATION", "NOTE":
		return SevLow
	default:
		return ""
	}
}

func firstLocation(locs []location) (path string, line int, url string) {
	if len(locs) == 0 {
		return "", 0, ""
	}
	pl := locs[0].PhysicalLocation
	return pl.ArtifactLocation.URI, pl.Region.StartLine, pl.ArtifactLocation.URI
}

// fingerprint prefers a tool-provided fingerprint (stable across runs — the
// basis for v2 dedup), falling back to a content hash.
func fingerprint(res *result, f Finding) string {
	if fp := anyValue(res.Fingerprints); fp != "" {
		return capField(fp)
	}
	if fp := anyValue(res.PartialFingerprints); fp != "" {
		return capField(fp)
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{
		f.Tool, f.RuleID, f.LocationPath, strconv.Itoa(f.LocationLine), f.Message,
	}, "|")))
	return fmt.Sprintf("%x", sum[:])
}

// anyValue returns a deterministic value from a fingerprint map (smallest key)
// so the same finding always yields the same fingerprint.
func anyValue(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	minKey := ""
	for k := range m {
		if minKey == "" || k < minKey {
			minKey = k
		}
	}
	return m[minKey]
}

func capStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func capField(s string) string { return capStr(s, maxFieldLen) }
