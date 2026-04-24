package admin

import (
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/store"
)

// Audit handles GET /api/v1/admin/audit. Returns the most recent
// events, filterable by query params:
//
//	?action=project.apply       — exact action match
//	?target_type=project        — exact target-type match
//	?actor=alice                — actor_email ILIKE match
//	?actor_id=<uuid>            — exact actor id match
//	?limit=50                   — cap page size (default 100, max 500)
//
// Admin-only. The admin UI renders the event stream + filters;
// CLI operators can also curl this to tail activity.
func (h *Handler) Audit(w http.ResponseWriter, r *http.Request) {
	if !methodGET(w, r) {
		return
	}
	q := r.URL.Query()
	limit := int32(100)
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			http.Error(w, "limit must be a positive integer", http.StatusBadRequest)
			return
		}
		if n > 500 {
			n = 500 // cap to protect the list endpoint
		}
		limit = int32(n)
	}
	var offset int32
	if raw := q.Get("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			http.Error(w, "offset must be >= 0", http.StatusBadRequest)
			return
		}
		offset = int32(n)
	}

	f := store.ListAuditEventsFilter{
		Action:     q.Get("action"),
		TargetType: q.Get("target_type"),
		ActorEmail: q.Get("actor"),
		Limit:      limit,
		Offset:     offset,
	}
	if raw := q.Get("actor_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			http.Error(w, "actor_id must be a UUID", http.StatusBadRequest)
			return
		}
		f.ActorID = id
	}

	page, err := h.store.ListAuditEvents(r.Context(), f)
	if err != nil {
		h.log.Error("admin: list audit events", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"events": page.Events,
		"total":  page.Total,
		"limit":  page.Limit,
		"offset": page.Offset,
	})
}
