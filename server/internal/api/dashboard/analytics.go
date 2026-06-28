package dashboard

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// LabelKeys handles GET /api/v1/analytics/label-keys — the distinct project
// label keys available as a "group by" dimension.
func (h *Handler) LabelKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := h.store.LabelKeys(r.Context())
	if err != nil {
		h.log.Error("analytics: label keys", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if keys == nil {
		keys = []string{}
	}
	writeAnalyticsJSON(w, map[string]any{"keys": keys})
}

// DoraRollup handles GET /api/v1/analytics/dora?key=<labelKey>&window_days=<N>.
// Returns the four DORA metrics for each value of the label key over the
// trailing window. `key` is required.
func (h *Handler) DoraRollup(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if key == "" {
		http.Error(w, "key is required", http.StatusBadRequest)
		return
	}
	windowDays := 30
	if v := r.URL.Query().Get("window_days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 365 {
			http.Error(w, "window_days must be 1..365", http.StatusBadRequest)
			return
		}
		windowDays = n
	}

	groups, err := h.store.DoraRollup(r.Context(), key, windowDays)
	if err != nil {
		h.log.Error("analytics: dora rollup", "key", key, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeAnalyticsJSON(w, map[string]any{
		"key":         key,
		"window_days": windowDays,
		"groups":      groups,
	})
}

func writeAnalyticsJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}
