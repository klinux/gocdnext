package runs

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// SecurityFindings handles GET /api/v1/runs/{id}/security-findings — the run's
// security snapshot: open counts (identity-deduped), accepted, and the findings
// "new in this change" vs the base branch (for PR runs).
//
// Authorization (explicit decision): this is a run-scoped read behind RequireAuth
// only, exactly like /runs/{id}/coverage, /tests and the logs — gocdnext's RBAC
// is global roles (admin/maintainer/viewer), with no per-project read ACL, so an
// authenticated viewer can read any run's data. Security findings are more
// sensitive than coverage, but isolating only this endpoint would be inconsistent
// and there's no per-project access primitive to check against. If per-project
// read isolation is ever introduced, resolve run -> project here and deny
// cross-project (with a test) — the same hook coverage/tests/logs would need.
func (h *Handler) SecurityFindings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	runID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	sec, err := h.store.RunSecuritySummary(r.Context(), runID)
	if err != nil {
		h.log.Error("security: by run", "run_id", runID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(sec)
}
