package dashboard_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAnalytics_DoraValidationAndShape(t *testing.T) {
	h, _ := newHandler(t)

	// key is required.
	rr := httptest.NewRecorder()
	h.DoraRollup(rr, httptest.NewRequest(http.MethodGet, "/api/v1/analytics/dora", nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("missing key status = %d, want 400", rr.Code)
	}

	// window_days out of range.
	rr = httptest.NewRecorder()
	h.DoraRollup(rr, httptest.NewRequest(http.MethodGet, "/api/v1/analytics/dora?key=team&window_days=999", nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("bad window status = %d, want 400", rr.Code)
	}

	// Label keys use ':' as display separator and are bounded.
	rr = httptest.NewRecorder()
	h.DoraRollup(rr, httptest.NewRequest(http.MethodGet, "/api/v1/analytics/dora?key=team:backend", nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("bad key status = %d, want 400", rr.Code)
	}

	// Happy path (no data) → 200 with an empty groups array, echoing inputs.
	rr = httptest.NewRecorder()
	h.DoraRollup(rr, httptest.NewRequest(http.MethodGet, "/api/v1/analytics/dora?key=team&window_days=14", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Key        string `json:"key"`
		WindowDays int    `json:"window_days"`
		Groups     []struct {
			Group string `json:"group"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Key != "team" || body.WindowDays != 14 {
		t.Fatalf("echo = %+v", body)
	}
}

func TestAnalytics_Overview(t *testing.T) {
	h, _ := newHandler(t)

	// key required.
	rr := httptest.NewRecorder()
	h.Overview(rr, httptest.NewRequest(http.MethodGet, "/api/v1/analytics/dora/overview", nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("missing key status = %d, want 400", rr.Code)
	}

	// Happy path (no data) → 200 echoing key/window with empty teams + current/prior.
	rr = httptest.NewRecorder()
	h.Overview(rr, httptest.NewRequest(http.MethodGet, "/api/v1/analytics/dora/overview?key=team&window_days=14", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Key        string `json:"key"`
		WindowDays int    `json:"window_days"`
		Current    struct {
			DeploysTotal int64 `json:"deploys_total"`
		} `json:"current"`
		Prior struct {
			DeploysTotal int64 `json:"deploys_total"`
		} `json:"prior"`
		Teams []any `json:"teams"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Key != "team" || body.WindowDays != 14 {
		t.Fatalf("echo = %+v", body)
	}
}

func TestAnalytics_Reliability(t *testing.T) {
	h, _ := newHandler(t)

	// key required.
	rr := httptest.NewRecorder()
	h.Reliability(rr, httptest.NewRequest(http.MethodGet, "/api/v1/analytics/reliability", nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("missing key status = %d, want 400", rr.Code)
	}

	// window_days out of range.
	rr = httptest.NewRecorder()
	h.Reliability(rr, httptest.NewRequest(http.MethodGet, "/api/v1/analytics/reliability?key=team&window_days=0", nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("bad window status = %d, want 400", rr.Code)
	}

	// Happy path (no data) → 200 echoing key/window with empty groups + hotspots.
	rr = httptest.NewRecorder()
	h.Reliability(rr, httptest.NewRequest(http.MethodGet, "/api/v1/analytics/reliability?key=team&window_days=14", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Key        string `json:"key"`
		WindowDays int    `json:"window_days"`
		Groups     []any  `json:"groups"`
		Hotspots   []any  `json:"hotspots"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Key != "team" || body.WindowDays != 14 {
		t.Fatalf("echo = %+v", body)
	}
}

func TestAnalytics_LabelKeys(t *testing.T) {
	h, _ := newHandler(t)
	rr := httptest.NewRecorder()
	h.LabelKeys(rr, httptest.NewRequest(http.MethodGet, "/api/v1/analytics/label-keys", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if !json.Valid(rr.Body.Bytes()) || !strings.Contains(rr.Body.String(), `"keys"`) {
		t.Fatalf("body = %s", rr.Body.String())
	}
}

func TestAnalytics_Environments(t *testing.T) {
	h, _ := newHandler(t)

	// key required.
	rr := httptest.NewRecorder()
	h.Environments(rr, httptest.NewRequest(http.MethodGet, "/api/v1/analytics/environments", nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("missing key status = %d, want 400", rr.Code)
	}

	// Happy path (no data) → 200 with an empty environments array.
	rr = httptest.NewRecorder()
	h.Environments(rr, httptest.NewRequest(http.MethodGet, "/api/v1/analytics/environments?key=team", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if !json.Valid(rr.Body.Bytes()) || !strings.Contains(rr.Body.String(), `"environments"`) {
		t.Fatalf("body = %s", rr.Body.String())
	}
}

func TestAnalytics_OverviewEnvironmentEcho(t *testing.T) {
	h, _ := newHandler(t)
	rr := httptest.NewRecorder()
	h.Overview(rr, httptest.NewRequest(http.MethodGet, "/api/v1/analytics/dora/overview?key=team&environment=prod", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"environment":"prod"`) {
		t.Fatalf("environment not echoed: %s", rr.Body.String())
	}
}
