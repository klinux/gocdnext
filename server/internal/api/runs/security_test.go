package runs_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/api/runs"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func TestSecurityFindings_Endpoint(t *testing.T) {
	pool := dbtest.SetupPool(t)
	h := runs.NewHandler(store.New(pool), slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := chi.NewRouter()
	r.Get("/api/v1/runs/{id}/security-findings", h.SecurityFindings)

	// Invalid id → 400.
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/runs/not-a-uuid/security-findings", nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("bad id status = %d, want 400", rr.Code)
	}

	// Unknown (valid) run → 200, lenient empty snapshot (mirrors coverage).
	rr = httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodGet,
		"/api/v1/runs/"+uuid.NewString()+"/security-findings", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("unknown run status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		HasScans       bool `json:"has_scans"`
		DeltaAvailable bool `json:"delta_available"`
		NewInChange    []struct {
			RuleID string `json:"rule_id"`
		} `json:"new_in_change"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.HasScans || body.DeltaAvailable || len(body.NewInChange) != 0 {
		t.Fatalf("unknown run must be an empty snapshot, got %+v", body)
	}
}
