package projects

import (
	"encoding/json"
	"net/http"
)

// List handles GET /api/v1/projects. Returns the list of every project with
// pipeline counts and most-recent run timestamp — the home-page feed for the
// dashboard.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	projects, err := h.store.ListProjects(r.Context())
	if err != nil {
		h.log.Error("list projects", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"projects": projects})
}
