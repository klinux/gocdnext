package sarif

import (
	"os"
	"strings"
	"testing"
)

func TestParse_Semgrep_SeverityFromRuleDefault(t *testing.T) {
	f, err := os.Open("testdata/semgrep.sarif")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	got, truncated, err := Parse(f)
	if err != nil || truncated {
		t.Fatalf("parse: err=%v truncated=%v", err, truncated)
	}
	if len(got) != 1 {
		t.Fatalf("findings = %d, want 1", len(got))
	}
	fnd := got[0]
	// No result.level + no CVSS → falls back to rule.defaultConfiguration.level
	// ("warning") → medium. Raw level is taken from the rule default too.
	if fnd.Tool != "Semgrep" || fnd.RuleID != "go.lang.security.audit.xss" {
		t.Fatalf("tool/rule = %+v", fnd)
	}
	if fnd.Severity != SevMedium {
		t.Errorf("severity = %q, want medium", fnd.Severity)
	}
	if fnd.Level != "warning" {
		t.Errorf("level = %q, want warning (from rule default)", fnd.Level)
	}
	if fnd.LocationPath != "web/handler.go" || fnd.LocationLine != 42 {
		t.Errorf("location = %q:%d", fnd.LocationPath, fnd.LocationLine)
	}
	if fnd.Fingerprint != "abc123def456" {
		t.Errorf("fingerprint = %q, want the tool partialFingerprint", fnd.Fingerprint)
	}
}

func TestParse_Trivy_CVSSCritical(t *testing.T) {
	f, err := os.Open("testdata/trivy.sarif")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	got, _, err := Parse(f)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("findings = %d, want 1", len(got))
	}
	fnd := got[0]
	if fnd.Tool != "Trivy" || fnd.RuleID != "CVE-2023-1234" {
		t.Fatalf("tool/rule = %+v", fnd)
	}
	// security-severity 9.8 → critical (overrides the "error" level which alone
	// would be high).
	if fnd.Severity != SevCritical {
		t.Errorf("severity = %q, want critical (CVSS 9.8)", fnd.Severity)
	}
	if fnd.Level != "error" {
		t.Errorf("level = %q, want error", fnd.Level)
	}
	// No tool fingerprints → content hash (64-hex sha256).
	if len(fnd.Fingerprint) != 64 {
		t.Errorf("fingerprint = %q, want a 64-char content hash", fnd.Fingerprint)
	}
}

func TestParse_SeverityResolutionOrder(t *testing.T) {
	const doc = `{"runs":[{"tool":{"driver":{"name":"X","rules":[
      {"id":"r-cvss","properties":{"security-severity":"7.5","severity":"LOW"}},
      {"id":"r-level","defaultConfiguration":{"level":"error"}}
    ]}},"results":[
      {"ruleId":"r-cvss","level":"note","message":{"text":"m1"}},
      {"ruleId":"r-level","message":{"text":"m2"}},
      {"ruleId":"unknown-rule","level":"error","message":{"text":"m3"}}
    ]}]}`
	got, _, err := Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("findings = %d, want 3", len(got))
	}
	// rule CVSS 7.5 wins over result.level=note → high.
	if got[0].Severity != SevHigh {
		t.Errorf("r-cvss severity = %q, want high (rule CVSS beats note level)", got[0].Severity)
	}
	// no level on result → rule default "error" → high.
	if got[1].Severity != SevHigh || got[1].Level != "error" {
		t.Errorf("r-level = %q/%q, want high/error", got[1].Severity, got[1].Level)
	}
	// unknown rule, result.level=error → high.
	if got[2].Severity != SevHigh {
		t.Errorf("unknown-rule severity = %q, want high", got[2].Severity)
	}
}

func TestParse_InvalidJSON(t *testing.T) {
	if _, _, err := Parse(strings.NewReader("not json")); err == nil {
		t.Fatal("expected error on invalid JSON")
	}
}

func TestParse_TruncatesAtCap(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"runs":[{"tool":{"driver":{"name":"X"}},"results":[`)
	for i := 0; i < MaxFindings+10; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"ruleId":"r","level":"warning","message":{"text":"m"}}`)
	}
	b.WriteString(`]}]}`)

	got, truncated, err := Parse(strings.NewReader(b.String()))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !truncated || len(got) != MaxFindings {
		t.Fatalf("got %d findings, truncated=%v; want %d + truncated", len(got), truncated, MaxFindings)
	}
}
