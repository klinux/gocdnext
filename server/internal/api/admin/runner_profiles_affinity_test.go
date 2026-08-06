package admin_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
)

// Admin POST → GET round-trip preserves preferred_node_affinity across the
// strict decode + store + JSONB read.
func TestRunnerProfiles_Affinity_RoundTrip(t *testing.T) {
	_, _, srv := newRunnerProfileHandler(t)

	body := bytes.NewBufferString(`{
        "name": "spot-pool",
        "engine": "kubernetes",
        "preferred_node_affinity": [
            {"weight": 100, "match_expressions": [
                {"key": "cloud.google.com/gke-spot", "operator": "In", "values": ["true"]}
            ]}
        ]
    }`)
	rr := request(srv, http.MethodPost, "/api/v1/admin/runner-profiles", body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body=%s", rr.Code, rr.Body.String())
	}

	rr = request(srv, http.MethodGet, "/api/v1/admin/runner-profiles", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status = %d", rr.Code)
	}
	var listed struct {
		Profiles []map[string]any `json:"profiles"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &listed)
	if len(listed.Profiles) != 1 {
		t.Fatalf("len = %d", len(listed.Profiles))
	}
	aff, _ := listed.Profiles[0]["preferred_node_affinity"].([]any)
	if len(aff) != 1 {
		t.Fatalf("affinity len = %d", len(aff))
	}
	term, _ := aff[0].(map[string]any)
	if term["weight"] != float64(100) {
		t.Errorf("weight = %v, want 100", term["weight"])
	}
	me, _ := term["match_expressions"].([]any)
	if len(me) != 1 {
		t.Fatalf("match_expressions len = %d", len(me))
	}
	expr, _ := me[0].(map[string]any)
	if expr["key"] != "cloud.google.com/gke-spot" || expr["operator"] != "In" {
		t.Errorf("expr lost: %+v", expr)
	}
}

// Strict JSON: an unknown field is a 400, not silently dropped.
func TestRunnerProfiles_RejectsUnknownField(t *testing.T) {
	_, _, srv := newRunnerProfileHandler(t)
	body := bytes.NewBufferString(
		`{"name":"x","engine":"kubernetes","preferred_node_affinty":[]}`)
	rr := request(srv, http.MethodPost, "/api/v1/admin/runner-profiles", body)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("unknown-field status = %d, want 400 (body=%s)", rr.Code, rr.Body.String())
	}
}

// Strict JSON: trailing content / a second object after the payload is a 400.
func TestRunnerProfiles_RejectsTrailingJSON(t *testing.T) {
	_, _, srv := newRunnerProfileHandler(t)
	body := bytes.NewBufferString(
		`{"name":"x","engine":"kubernetes"}{"name":"y","engine":"kubernetes"}`)
	rr := request(srv, http.MethodPost, "/api/v1/admin/runner-profiles", body)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("trailing-json status = %d, want 400 (body=%s)", rr.Code, rr.Body.String())
	}
}
