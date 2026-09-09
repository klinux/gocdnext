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
