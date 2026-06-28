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
