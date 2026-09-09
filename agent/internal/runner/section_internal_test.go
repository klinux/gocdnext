package runner

import (
	"strings"
	"sync/atomic"
	"testing"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
)

// A phase divider (#277) is one stdout line carrying the section name and a
// visual rule — readable raw (`grep '────'`) with no collapsing.
func TestEmitSection_DividerCarriesNameOnStdout(t *testing.T) {
	var got []*gocdnextv1.LogLine
	r := New(Config{Send: func(m *gocdnextv1.AgentMessage) {
		if l := m.GetLog(); l != nil {
			got = append(got, l)
		}
	}})

	var seq atomic.Int64
	a := &gocdnextv1.JobAssignment{RunId: "r", JobId: "j"}
	r.emitSection(a, &seq, "RUN")

	if len(got) != 1 {
		t.Fatalf("emitSection emitted %d log lines, want exactly 1", len(got))
	}
	line := got[0]
	if line.GetStream() != "stdout" {
		t.Errorf("stream = %q, want stdout", line.GetStream())
	}
	if !strings.Contains(line.GetText(), "RUN") {
		t.Errorf("divider text %q missing the section name", line.GetText())
	}
	if !strings.Contains(line.GetText(), "────") {
		t.Errorf("divider text %q missing the rule so it can't be grepped as a boundary", line.GetText())
	}
}

func TestHasPostJobWork(t *testing.T) {
	tests := []struct {
		name       string
		a          *gocdnextv1.JobAssignment
		success    bool
		cacheWired bool
		want       bool
	}{
		{"empty job → no post-job divider", &gocdnextv1.JobAssignment{}, true, true, false},
		{"test reports", &gocdnextv1.JobAssignment{TestReports: []string{"**/TEST-*.xml"}}, true, false, true},
		{"coverage", &gocdnextv1.JobAssignment{CoverageReport: &gocdnextv1.CoverageReportSpec{}}, true, false, true},
		{"cache only + wired, success", &gocdnextv1.JobAssignment{Caches: []*gocdnextv1.CacheEntry{{Key: "k"}}}, true, true, true},
		{"cache only, NOT wired", &gocdnextv1.JobAssignment{Caches: []*gocdnextv1.CacheEntry{{Key: "k"}}}, true, false, false},
		{"cache only, wired but FAILURE (no store)", &gocdnextv1.JobAssignment{Caches: []*gocdnextv1.CacheEntry{{Key: "k"}}}, false, true, false},
		{"artifacts default when, success", &gocdnextv1.JobAssignment{ArtifactPaths: []string{"dist/*"}}, true, false, true},
		{"artifacts default when, FAILURE (no ship)", &gocdnextv1.JobAssignment{ArtifactPaths: []string{"dist/*"}}, false, false, false},
		{"artifacts on_failure, FAILURE (ships)", &gocdnextv1.JobAssignment{ArtifactPaths: []string{"dist/*"}, ArtifactsWhen: "on_failure"}, false, false, true},
		{"artifacts always, FAILURE (ships)", &gocdnextv1.JobAssignment{ArtifactPaths: []string{"dist/*"}, ArtifactsWhen: "always"}, false, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasPostJobWork(tt.a, tt.success, tt.cacheWired); got != tt.want {
				t.Errorf("hasPostJobWork = %v, want %v", got, tt.want)
			}
		})
	}
}
