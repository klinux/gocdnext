package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func scrape(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("scrape status = %d", rec.Code)
	}
	return rec.Body.String()
}

func TestHandlerExposesInstruments(t *testing.T) {
	LogLinesDropped.Inc()
	ObserveJobPhase("prep", 2*time.Second)

	body := scrape(t)
	for _, want := range []string{
		"gocdnext_agent_log_lines_dropped_total",
		"gocdnext_agent_job_duration_seconds",
		`phase="prep"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("scrape missing %q\n%s", want, body)
		}
	}
}

func TestObserveJobPhaseClampsUnknownLabel(t *testing.T) {
	ObserveJobPhase("totally-made-up", time.Second)
	body := scrape(t)
	if strings.Contains(body, `phase="totally-made-up"`) {
		t.Fatalf("unexpected free-form phase label leaked into a metric:\n%s", body)
	}
	if !strings.Contains(body, `phase="unknown"`) {
		t.Fatalf("unknown phase not collapsed to \"unknown\":\n%s", body)
	}
}

func TestPhaseLabelClosedSet(t *testing.T) {
	for _, p := range []string{"prep", "task", "post_job"} {
		if got := phaseLabel(p); got != p {
			t.Fatalf("phaseLabel(%q) = %q, want passthrough", p, got)
		}
	}
	if got := phaseLabel("artifacts"); got != "unknown" {
		t.Fatalf("phaseLabel(unexpected) = %q, want unknown", got)
	}
}
