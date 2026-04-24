package admin

import (
	"net/http"
	"strconv"
	"time"

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
//	?from=2026-04-24T00:00:00Z  — inclusive lower bound (RFC3339 or date)
//	?to=2026-04-25T00:00:00Z    — exclusive upper bound (RFC3339 or date)
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
	if raw := q.Get("from"); raw != "" {
		t, err := parseAuditTime(raw)
		if err != nil {
			http.Error(w, "from must be RFC3339 or YYYY-MM-DD", http.StatusBadRequest)
			return
		}
		f.FromAt = t
	}
	if raw := q.Get("to"); raw != "" {
		t, err := parseAuditTime(raw)
		if err != nil {
			http.Error(w, "to must be RFC3339 or YYYY-MM-DD", http.StatusBadRequest)
			return
		}
		f.ToAt = t
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

// parseAuditTime accepts two common shapes the admin UI sends:
//
//   - full RFC3339 ("2026-04-24T00:00:00Z") — what the `<input
//     type="datetime-local">` submits after a timezone offset is
//     applied client-side.
//   - bare date ("2026-04-24") — what `<input type="date">`
//     submits. Interpreted as 00:00:00 UTC; the half-open window
//     in the SQL means a caller passing ("2026-04-24",
//     "2026-04-25") hits exactly day 24.
//
// Returns an error on any other shape so junk params don't
// silently widen the window.
func parseAuditTime(raw string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, nil
	}
	return time.Parse("2006-01-02", raw)
}
